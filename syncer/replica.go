package syncer

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sync/atomic"
	"time"

	"github.com/ikenchina/redis-GunYu/pkg/cluster"
	"github.com/ikenchina/redis-GunYu/pkg/io/pipe"
	"github.com/ikenchina/redis-GunYu/pkg/log"
	pb "github.com/ikenchina/redis-GunYu/pkg/replica/golang"
	usync "github.com/ikenchina/redis-GunYu/pkg/sync"

	"golang.org/x/exp/slices"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type ReplicaLeader struct {
	start   atomic.Bool
	logger  log.Logger
	input   Input
	channel Channel
}

func NewReplicaLeader(input Input, channel Channel) *ReplicaLeader {
	replica := &ReplicaLeader{
		logger:  log.WithLogger("[ReplicaLeader] "),
		input:   input,
		channel: channel,
	}
	return replica
}

func (rl *ReplicaLeader) Start() {
	rl.start.Store(true)
}

func (rl *ReplicaLeader) Stop() {
}

func (rl *ReplicaLeader) handleError(stream pb.ReplService_SyncServer, err error, code pb.SyncResponse_Code, msg string, runId string) error {
	if err != nil {
		if code >= pb.SyncResponse_ERROR {
			rl.logger.Errorf("%s", err.Error())
		} else {
			rl.logger.Warnf("%s", err.Error())
		}
	}
	stream.Send(&pb.SyncResponse{
		Code: code, Meta: &pb.SyncResponse_Meta{Msg: msg, RunId: runId},
	})
	return err
}

func (rl *ReplicaLeader) selfInspection(stream pb.ReplService_SyncServer) error {
	if !rl.start.Load() {
		return fmt.Errorf("replica is not running")
	}

	// self inspection
	//
	// fault -> error -> failure
	runIds := rl.input.RunIds()
	if len(runIds) == 0 {
		err := errors.Join(ErrRestart, fmt.Errorf("no input ids"))
		return rl.handleError(stream, err, pb.SyncResponse_FAILURE, "internal error", "")
	}

	// check channel run id
	channelRunId := rl.channel.RunId()
	if !slices.Contains(runIds, channelRunId) || runIds[0] != channelRunId {
		err := errors.Join(ErrRestart, fmt.Errorf("channel run id is stale : input_run_ids(%v), channel_run_id(%s)", runIds, channelRunId))
		return rl.handleError(stream, err, pb.SyncResponse_FAILURE, "internal error", "")
	}
	return nil
}

func (rl *ReplicaLeader) Handle(wait usync.WaitCloser, req *pb.SyncRequest, stream pb.ReplService_SyncServer) error {

	err := rl.selfInspection(stream)
	if err != nil {
		return err
	}

	followerRunId := req.GetNode().GetRunId()
	followerOffset := req.GetOffset()
	inputRunIds := rl.input.RunIds()

	// 1. protoHandShake : send run id to follower
	sp, _ := rl.channel.StartPoint(nil)
	if followerRunId == "" || followerRunId == "?" {
		err := stream.Send(&pb.SyncResponse{
			Code: pb.SyncResponse_META, Meta: &pb.SyncResponse_Meta{RunId: sp.RunId},
			Offset: sp.Offset,
		})
		if err != nil {
			return rl.handleError(stream, err, pb.SyncResponse_FAULT, "internal error", "")
		}
		return nil
	}

	if inputRunIds[0] != followerRunId {
		// @TODO a corner case : replica get a newer run id, master is stale
		err := fmt.Errorf("run id is stale : input_run_ids(%v), replica_run_id(%s)", inputRunIds, followerRunId)
		return rl.handleError(stream, err, pb.SyncResponse_ERROR, "internal error", "")
	}

	// if follower's offset is newer, try to hand over the leadership
	if followerOffset-sp.Offset > 0 { // if offset is negative, still workable
		rl.logger.Infof("peer's offset is newer, hand over leadership : peer(%d), leader(%d)", followerOffset, sp.Offset)
		err := stream.Send(&pb.SyncResponse{
			Code:   pb.SyncResponse_HANDOVER,
			Meta:   &pb.SyncResponse_Meta{RunId: sp.RunId},
			Offset: sp.Offset,
		})
		if err != nil {
			return err
		}
		return ErrLeaderHandover
	}

	return rl.sendData(wait, req, stream, StartPoint{RunId: followerRunId, Offset: followerOffset}, sp)
}

func (rl *ReplicaLeader) sendData(wait usync.WaitCloser, req *pb.SyncRequest, stream pb.ReplService_SyncServer, reqSp StartPoint, channelSp StartPoint) error {

	// the offset of follower is invalid
	if !rl.channel.IsValidOffset(Offset{RunId: reqSp.RunId, Offset: reqSp.Offset}) {
		reqSp.Offset = channelSp.Offset
	}

	// pump data from storer
	reader, err := rl.channel.NewReader(Offset{
		RunId:  reqSp.RunId,
		Offset: reqSp.Offset,
	})
	if err != nil {
		err = errors.Join(fmt.Errorf("channel.NewReader error : offset(%s:%d), error(%w)", reqSp.RunId, reqSp.Offset, err))
		return rl.handleError(stream, err, pb.SyncResponse_ERROR, "internal error", "")
	}

	reader.Start(wait)
	ioReader := reader.IoReader()
	offset := reqSp.Offset

	// 2.1 meta sync
	if err := stream.Send(&pb.SyncResponse{
		Code:   pb.SyncResponse_META,
		Meta:   &pb.SyncResponse_Meta{Aof: reader.IsAof()},
		Offset: reader.Left(), Size: reader.Size(),
	}); err != nil {
		return rl.handleError(stream, err, pb.SyncResponse_FAULT, err.Error(), "")
	}

	rl.logger.Infof("start to send data to follower : offset(%d), size(%d)", offset, reader.Size())

	// 2.2 send data
	sendSize := uint64(reader.Size()) // -1 mean max
	if sendSize == 0 {
		sendSize = math.MaxInt64
	}
	for !wait.IsClosed() && sendSize > 0 {
		// @TODO metrics

		// @TODO @OPTIMIZE reuse, array of []byte, notice that stream.Send is async
		buf := make([]byte, 1024*4)
		n, err := ioReader.Read(buf)
		if err != nil {
			rl.logger.Errorf("reader error : %v", err)
			if errors.Is(err, io.EOF) {
				return stream.Send(&pb.SyncResponse{
					Code: pb.SyncResponse_CONTINUE, Offset: offset + int64(n), Size: int64(n),
					Data: buf,
				})
			}
			return rl.handleError(stream, err, pb.SyncResponse_FAULT, "reader error", "")
		}

		buf = buf[:n]
		if err = stream.Send(&pb.SyncResponse{
			Code: pb.SyncResponse_CONTINUE, Offset: offset + int64(n), Size: int64(n),
			Data: buf,
		}); err != nil {
			return rl.handleError(stream, err, pb.SyncResponse_FAULT, "reader error", "")
		}
		offset += int64(n)
		sendSize -= uint64(n)
	}

	return nil
}

// follower

type ReplicaFollower struct {
	wait         usync.WaitCloser
	logger       log.Logger
	inputAddress string
	input        Input
	channel      Channel
	leader       *cluster.RoleInfo
}

func NewReplicaFollower(inputAddress string, input Input, channel Channel, leader *cluster.RoleInfo) *ReplicaFollower {
	replica := &ReplicaFollower{
		logger:       log.WithLogger("[ReplicaFollower] "),
		wait:         usync.NewWaitCloser(nil),
		input:        input,
		channel:      channel,
		leader:       leader,
		inputAddress: inputAddress,
	}
	return replica
}

func (rf *ReplicaFollower) Run() error {
	conn, err := rf.newGrpcConn(rf.leader)
	if err != nil {
		return err
	}
	defer conn.Close()

	cli := pb.NewReplServiceClient(conn)

	state := 1
	var leaderSp, followerSp StartPoint
	var stream pb.ReplService_SyncClient
	var resp *pb.SyncResponse

	for !rf.wait.IsClosed() {
		switch state {
		case 1: // shake
			leaderSp, err = rf.protoHandShake(cli)
		case 2: // prepare
			followerSp, err = rf.preSync(leaderSp)
		case 3: // meta sync
			stream, resp, err = rf.metaSync(followerSp, cli)
			if err == nil {
				if resp.GetMeta().GetAof() {
					state = 5
				} else {
					state = 4
				}
				continue
			}
		case 4: // rdb
			err = rf.rdbSync(followerSp, stream, resp)
			if err == nil {
				followerSp, err = rf.channel.StartPoint([]string{leaderSp.RunId})
				if err != nil {
					err = errors.Join(ErrRestart, fmt.Errorf("channel.StartPoint error : runId(%s), error(%v)", leaderSp.RunId, err))
					return err
				}
				state = 3 // meta sync
				continue
			}
		case 5: // aof
			err = rf.aofSync(followerSp, stream, resp)
		default:
			state = 1
			rf.wait.Sleep(3 * time.Second) // sleep and try again
		}
		if err == nil {
			state++
		} else {
			state = 1 // restart sync
			rf.logger.Errorf("RunFollower error : state(%d), error(%v)", state, err)
			if errors.Is(err, ErrBreak) || errors.Is(err, ErrRole) {
				return err
			}
			rf.wait.Sleep(3 * time.Second) // sleep and try again
		}
	}

	return nil
}

func (rf *ReplicaFollower) Stop() {
	rf.wait.Close(nil)
}

func (rf *ReplicaFollower) handleResp(err error, resp *pb.SyncResponse) error {
	if err != nil {
		rf.logger.Errorf("%s", err.Error())
		return err
	}
	if resp != nil {
		if resp.GetCode() == pb.SyncResponse_FAILURE { // propagation
			err = errors.Join(ErrRestart, fmt.Errorf("code is failure : %s", resp.GetMeta().GetMsg()))
		} else if resp.GetCode() == pb.SyncResponse_ERROR {
			err = fmt.Errorf("code is error : %s", resp.GetMeta().GetMsg())
		} else if resp.GetCode() == pb.SyncResponse_FAULT {
			err = fmt.Errorf("code is fault : %s", resp.GetMeta().GetMsg())
		} else if resp.GetCode() == pb.SyncResponse_HANDOVER {
			err = fmt.Errorf("takeover leadership : %w, leader(%d)", ErrLeaderTakeover, resp.GetOffset())
		}
	}
	return err
}

func (rf *ReplicaFollower) protoHandShake(cli pb.ReplServiceClient) (sp StartPoint, err error) {
	// 1. get run id and offset
	var stream pb.ReplService_SyncClient
	stream, err = cli.Sync(rf.wait.Context(), &pb.SyncRequest{
		Node: &pb.Node{
			Address: rf.inputAddress,
		},
	})
	if err = rf.handleResp(err, nil); err != nil {
		return
	}
	var resp *pb.SyncResponse
	resp, err = stream.Recv()
	if err = rf.handleResp(err, resp); err != nil {
		return
	}
	sp.RunId = resp.GetMeta().GetRunId()
	if sp.RunId == "" {
		err = rf.handleResp(errors.New("empty run id"), nil)
		return
	}
	sp.Offset = resp.GetOffset()
	return
}

func (rf *ReplicaFollower) preSync(leaderSp StartPoint) (sp StartPoint, err error) {
	sp, err = rf.channel.StartPoint([]string{leaderSp.RunId})
	if err != nil {
		err = errors.Join(ErrRestart, fmt.Errorf("channel.StartPoint error : runId(%s), error(%v)", leaderSp.RunId, err))
		return
	}
	rf.logger.Infof("gap : leader(%v), follower(%v)", leaderSp, sp)

	if sp.IsInitial() {
		if err = rf.channel.SetRunId(leaderSp.RunId); err != nil {
			err = errors.Join(ErrRestart, err)
			return
		}
		sp.RunId = leaderSp.RunId
		return
	}

	// check gap
	gap := leaderSp.Offset - sp.Offset
	if gap > 0 {
		if gap > 100*1024*1024 { // @TODO gap < 0, truncate extra data
			if err = rf.channel.DelRunId(sp.RunId); err != nil {
				err = errors.Join(ErrRestart, err)
				return
			}
			sp.Offset = leaderSp.Offset
		}
		if err = rf.channel.SetRunId(leaderSp.RunId); err != nil {
			err = errors.Join(ErrRestart, err)
			return
		}
	}
	return
}

func (rf *ReplicaFollower) metaSync(sp StartPoint, cli pb.ReplServiceClient) (pb.ReplService_SyncClient, *pb.SyncResponse, error) {
	stream, err := cli.Sync(rf.wait.Context(), &pb.SyncRequest{
		Node:   &pb.Node{RunId: sp.RunId, Address: rf.inputAddress},
		Offset: sp.Offset,
	})
	if err = rf.handleResp(err, nil); err != nil {
		return nil, nil, err
	}
	resp, err := stream.Recv()
	if err = rf.handleResp(err, resp); err != nil {
		return nil, nil, err
	}
	return stream, resp, nil
}

func (rf *ReplicaFollower) rdbSync(followerSp StartPoint, stream pb.ReplService_SyncClient, resp *pb.SyncResponse) error {
	isAof := resp.GetMeta().GetAof()
	if isAof {
		return nil
	}
	left := resp.GetOffset()
	if left > followerSp.Offset {
		if err := rf.channel.DelRunId(followerSp.RunId); err != nil {
			return errors.Join(ErrRestart, err)
		}
		followerSp.Offset = left
		if err := rf.channel.SetRunId(followerSp.RunId); err != nil {
			return errors.Join(ErrRestart, err)
		}
	}

	rf.logger.Infof("start to sync rdb from leader : offset(%d), size(%d)", resp.GetOffset(), resp.GetSize())
	rdbSize := resp.GetSize()
	rdbWait := usync.NewWaitCloserFromParent(rf.wait, nil)
	piper, pipew := pipe.NewSize(1024 * 1024 * 10)
	reader := bufio.NewReaderSize(piper, 1024*64)
	writer, err := rf.channel.NewRdbWriter(reader, left, rdbSize)
	if err != nil {
		return err
	}

	rdbWait.WgAdd(1)
	usync.SafeGo(func() { // sync from leader
		defer rdbWait.WgDone()
		for !rdbWait.IsClosed() && rdbSize > 0 {
			resp, err = stream.Recv()
			if err = rf.handleResp(err, resp); err != nil {
				if errors.Is(err, io.EOF) && rdbSize == 0 {
					return
				}
				rdbWait.Close(err)
				return
			}
			chunk := resp.GetData()
			_, err = pipew.Write(chunk)
			if err != nil {
				rdbWait.Close(err)
				return
			}
			rdbSize -= int64(len(chunk))
		}
	}, func(i interface{}) { rdbWait.Close(fmt.Errorf("panic: %v", i)) })

	// rdb writer
	rdbWait.Close(writer.Run(rdbWait.Context()))

	rdbWait.WgWait()
	return rdbWait.Error()
}

func (rf *ReplicaFollower) aofSync(followerSp StartPoint, stream pb.ReplService_SyncClient, resp *pb.SyncResponse) error {

	sp, err := rf.channel.StartPoint([]string{followerSp.RunId})
	if err != nil {
		return errors.Join(ErrRestart, fmt.Errorf("channel.StartPoint error : startPoint(%v), error(%v)", followerSp, err))
	}

	left := resp.GetOffset()
	if left > sp.Offset {
		if err = rf.channel.DelRunId(followerSp.RunId); err != nil {
			return errors.Join(ErrRestart, err)
		}
		sp.Offset = left
		if err = rf.channel.SetRunId(followerSp.RunId); err != nil {
			return errors.Join(ErrRestart, err)
		}
	}

	rf.logger.Infof("start to sync aof from leader : offset(%d)", resp.GetOffset())

	aofWait := usync.NewWaitCloserFromParent(rf.wait, nil)
	piper, pipew := pipe.NewSize(1024 * 1024 * 10)
	reader := bufio.NewReaderSize(piper, 1024*64)
	writer, err := rf.channel.NewAofWritter(reader, resp.GetOffset())
	if err != nil {
		return err
	}
	aofSize := uint64(reader.Size())
	aofWait.WgAdd(1)
	usync.SafeGo(func() { // sync from leader
		defer aofWait.WgDone()
		for !aofWait.IsClosed() && aofSize > 0 {
			resp, err = stream.Recv()
			if err = rf.handleResp(err, resp); err != nil {
				if errors.Is(err, io.EOF) && aofSize == 0 {
					return
				}
				aofWait.Close(err)
				return
			}
			chunk := resp.GetData()
			_, err = pipew.Write(chunk)
			if err != nil {
				aofWait.Close(err)
				return
			}
			aofSize -= uint64(len(chunk))
		}
	}, func(i interface{}) { aofWait.Close(fmt.Errorf("panic: %v", i)) })

	// aof writer
	aofWait.Close(writer.Run(aofWait.Context()))

	aofWait.WgWait()
	return errors.Join(writer.Close(), aofWait.Error())
}

func (rf *ReplicaFollower) newGrpcConn(leader *cluster.RoleInfo) (*grpc.ClientConn, error) {

	var grpcOpts = []grpc.DialOption{
		grpc.WithChainUnaryInterceptor(ClientUnaryCallInterceptor(grpc.WaitForReady(true))),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	}
	ctx, cancel := context.WithTimeout(rf.wait.Context(), time.Duration(10*time.Second))
	defer cancel()

	conn, err := grpc.DialContext(ctx, leader.Address, grpcOpts...)
	if err != nil {
		rf.logger.Errorf("dial error : server(%s), error(%v)", leader.Address, err)
		return nil, err
	}
	return conn, nil
}