package redis

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/ikenchina/redis-GunYu/config"
	"github.com/ikenchina/redis-GunYu/pkg/log"
	"github.com/ikenchina/redis-GunYu/pkg/redis/client"
	"github.com/ikenchina/redis-GunYu/pkg/redis/client/common"
	"github.com/ikenchina/redis-GunYu/pkg/util"
)

func ParseKeyspace(content []byte) (map[int32]int64, error) {
	if !bytes.HasPrefix(content, []byte("# Keyspace")) {
		return nil, fmt.Errorf("invalid info Keyspace: %s", string(content))
	}

	lines := bytes.Split(content, []byte("\n"))
	reply := make(map[int32]int64)
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("db")) {
			// line "db0:keys=18,expires=0,avg_ttl=0"
			items := bytes.Split(line, []byte(":"))
			db, err := strconv.Atoi(string(items[0][2:]))
			if err != nil {
				return nil, err
			}
			nums := bytes.Split(items[1], []byte(","))
			if !bytes.HasPrefix(nums[0], []byte("keys=")) {
				return nil, fmt.Errorf("invalid info Keyspace: %s", string(content))
			}
			keysNum, err := strconv.ParseInt(string(nums[0][5:]), 10, 0)
			if err != nil {
				return nil, err
			}
			reply[int32(db)] = int64(keysNum)
		} // end true
	} // end for
	return reply, nil
}

func SelectDB(c client.Redis, db uint32) error {
	if c.RedisType() == config.RedisTypeCluster {
		return nil
	}
	err := c.Send("select", db)
	if err != nil {
		return err
	}

	ok, err := c.ReceiveString()
	if err != nil {
		return err
	}

	if ok != "OK" {
		return fmt.Errorf("select db(%d) error : reply(%s)", db, ok)
	}
	return nil
}

func lpush(cli client.Redis, key []byte, field []byte) error {
	_, err := common.Int64(cli.Do("lpush", string(key), string(field)))
	if err != nil {
		return fmt.Errorf("lpush command error : key(%s), error(%w)", key, err)
	}
	return nil
}

func rpush(cli client.Redis, key []byte, field []byte) error {
	_, err := common.Int64(cli.Do("rpush", string(key), string(field)))
	if err != nil {
		return fmt.Errorf("rpush command error : key(%s), error(%w)", key, err)
	}
	return nil
}

func Float64ToByte(float float64) string {
	return strconv.FormatFloat(float, 'f', -1, 64)
}

func zadd(cli client.Redis, key []byte, score []byte, member []byte) error {
	_, err := common.Int64(cli.Do("zadd", string(key), string(score), string(member)))
	if err != nil {
		return fmt.Errorf("zadd command error : key(%s), error(%w)", key, err)
	}
	return nil
}

func sadd(cli client.Redis, key []byte, member []byte) error {
	_, err := common.Int64(cli.Do("sadd", key, member))
	if err != nil {
		return fmt.Errorf("sadd command error : key(%s), error(%w)", key, err)
	}
	return nil
}

func hset(cli client.Redis, key []byte, field []byte, value []byte) error {
	_, err := common.Int64(cli.Do("hset", string(key), string(field), string(value)))
	if err != nil {
		return fmt.Errorf("hset command error : key(%s), error(%w)", key, err)
	}
	return nil
}

func set(cli client.Redis, key []byte, value []byte) error {
	s, err := common.String(cli.Do("set", string(key), string(value)))
	if err != nil {
		return fmt.Errorf("set command error : key(%s), error(%w)", key, err)
	}
	if s != "OK" {
		return fmt.Errorf("set command response is not ok : key(%s), resp(%s)", key, s)
	}
	return nil
}

func GetRedisVersion(cli client.Redis) (string, error) {
	infoStr, err := common.Bytes(cli.Do("info", "server"))
	if err != nil {
		return "", err
	}

	infoKV := ParseRedisInfo(infoStr)
	if value, ok := infoKV["redis_version"]; ok {
		return value, nil
	} else {
		return "", fmt.Errorf("miss redis version info")
	}
}

// parse single info field: "info server", "info keyspace"
func ParseRedisInfo(content []byte) map[string]string {
	result := make(map[string]string, 10)
	lines := bytes.Split(content, []byte("\r\n"))
	for i := 0; i < len(lines); i++ {
		items := bytes.SplitN(lines[i], []byte(":"), 2)
		if len(items) != 2 {
			continue
		}
		result[string(items[0])] = string(items[1])
	}
	return result
}

func GetAllClusterAddress(cli client.Redis) ([]string, []string, error) {
	masters, slaves, err := GetAllClusterNode(cli)
	if err != nil {
		return nil, nil, err
	}

	ms := []string{}
	ss := []string{}
	for _, m := range masters {
		ms = append(ms, m.Address)
	}
	for _, s := range slaves {
		ss = append(ss, s.Address)
	}

	return ms, ss, nil
}

func GetAllClusterNode(cli client.Redis) ([]*ClusterNodeInfo, []*ClusterNodeInfo, error) {
	ret, err := common.String(cli.Do("cluster", "nodes"))
	if err != nil {
		return nil, nil, err
	}

	nodeList := ParseClusterNode(util.StringToBytes(ret))
	masters := ClusterNodeChoose(nodeList, config.RedisRoleMaster)
	slaves := ClusterNodeChoose(nodeList, config.RedisRoleSlave)
	return masters, slaves, nil
}

func ClusterNodeChoose(input []*ClusterNodeInfo, role config.RedisRole) []*ClusterNodeInfo {
	ret := make([]*ClusterNodeInfo, 0, len(input))
	for _, ele := range input {
		if ele.Flags == config.RedisRoleMaster.String() && role == config.RedisRoleMaster ||
			ele.Flags == config.RedisRoleSlave.String() && role == config.RedisRoleSlave ||
			role == config.RedisRoleAll {
			ret = append(ret, ele)
		}
	}
	return ret
}

// [migrating slot]
// 69c810d7647462f477e290b6360b9aa038a9de2a 127.0.0.1:6300@16300 myself,master - 0 1700530970000 1 connected 0-1999 2001-5461 [3000->-23b4d0116117fab2a763df61c712afa8e4f9e7a8]
// ea1377484f4f2b45155b20497fab0e000a56b6ac 127.0.0.1:6301@16301 master - 0 1700530972000 0 connected 2000 5462-10922
// 23b4d0116117fab2a763df61c712afa8e4f9e7a8 127.0.0.1:6302@16302 master - 0 1700530972589 2 connected 10923-16383

// $1181
// dc60792a35b30e6319b5866af83e131237ae37a4 :0@0 master,noaddr - 1699408763709 1699408763705 14 disconnected 244-666 5463-6128 10924-11589
// 66f3b61e5fc02d8d58e8b4d58b40a0cd82d6d000 127.0.0.1:6312@16312 master - 0 1699408793761 9 connected 12175-16383
// 30150e71a32620bb2716de769a25537b97774bb0 127.0.0.1:6304@16304 master - 0 1699408791000 15 connected 0-243 667-1252 6129-6713 11590-12174
// 75ec6340807933b3b827f662d4847457f49007aa 127.0.0.1:6311@16311 slave 828c9cdfa7faec4f4d2fbc5342d09879510dc8b3 0 1699408791000 10 connected
// d41296ff6badbbfa36b05c90ceafed3c4583393a :0@0 slave,noaddr 66f3b61e5fc02d8d58e8b4d58b40a0cd82d6d000 1699408763709 1699408763706 9 disconnected
// 828c9cdfa7faec4f4d2fbc5342d09879510dc8b3 :0@0 master,noaddr - 1699408763709 1699408763705 10 disconnected 6714-10922
// 3de1044bfe52eab099e956f146bde4a1278b185e 127.0.0.1:6303@16303 myself,slave 166585d6a8976b203f80897d6deec69607457eb3 0 1699408793000 16 connected
// 166585d6a8976b203f80897d6deec69607457eb3 127.0.0.1:6310@16310 master - 0 1699408792000 16 connected 1253-5462 10923
// cc6572e1b59efeec31d79fe532acd144947c99ce 127.0.0.1:6314@16314 slave 30150e71a32620bb2716de769a25537b97774bb0 0 1699408792755 15 connected
func ParseClusterNode(content []byte) []*ClusterNodeInfo {
	lines := bytes.Split(content, []byte("\n"))
	ret := make([]*ClusterNodeInfo, 0, len(lines))
	for _, line := range lines {
		if bytes.Equal(line, []byte{}) {
			continue
		}

		items := bytes.Split(line, []byte(" "))

		address := bytes.Split(items[1], []byte{'@'})
		flag := bytes.Split(items[2], []byte{','})
		var role string
		if len(flag) > 1 {
			role = string(flag[1])
		} else {
			role = string(flag[0])
		}

		ret = append(ret, &ClusterNodeInfo{
			Id:          string(items[0]),
			Address:     string(address[0]),
			Flags:       role,
			Master:      string(items[3]),
			PingSent:    string(items[4]),
			PongRecv:    string(items[5]),
			ConfigEpoch: string(items[6]),
			LinkStat:    string(items[7]),
		})
	}
	return ret
}

var (
	spaceBs = []byte(" ")
)

func splitLineToArgs(line []byte) [][]byte {
	ret := make([][]byte, 0)
	items := bytes.Split(line, spaceBs)
	for i := 0; i < len(items); i++ {
		if bytes.Equal(items[i], spaceBs) {
			continue
		}
		ret = append(ret, items[i])
	}
	return ret
}

func GetClusterIsMigrating(cli client.Redis) (bool, error) {
	ret, err := common.String(cli.Do("cluster", "nodes"))
	if err != nil {
		return false, err
	}

	content := (util.StringToBytes(ret))
	lines := bytes.Split(content, []byte("\n"))

	for _, line := range lines {
		if bytes.Equal(line, []byte{}) {
			continue
		}
		items := splitLineToArgs(line)
		if len(items) <= 8 {
			log.Errorf("the number of args is less than 9 : line(%s)", line)
			continue
		}

		for i := 8; i < len(items); i++ {
			item := items[i]
			if item[0] == '[' {
				return true, nil
			}
		}
	}
	return false, nil
}

type ClusterNodeInfo struct {
	Id          string
	Address     string
	Flags       string
	Master      string
	PingSent    string
	PongRecv    string
	ConfigEpoch string
	LinkStat    string
	Slot        string
}

func GetAllClusterShard(cli client.Redis) ([]*config.RedisClusterShard, error) {
	ret, err := cli.Do("cluster", "shards")
	if err != nil {
		return nil, err
	}

	cShards := []*config.RedisClusterShard{}

	shards := ret.([]interface{})
	for _, shard := range shards {
		kvs := shard.([]interface{})
		key := ""
		cShard := &config.RedisClusterShard{}
		for _, kv := range kvs {
			switch tv := kv.(type) {
			case string:
				key = tv
			case []byte:
				key = string(tv)
			case []interface{}:
				if key == "slots" {
					if len(tv)%2 == 0 {
						for i := 0; i < len(tv); i += 2 {
							cShard.Slots.Ranges = append(cShard.Slots.Ranges, config.RedisSlotRange{
								Left:  int(tv[i].(int64)),
								Right: int(tv[i+1].(int64)),
							})
						}
					} else {

					}
				} else if key == "nodes" {
					for _, node := range tv {
						cNode := config.RedisNode{}
						eleKvs := node.([]interface{})
						for i := 0; i < len(eleKvs); i += 2 {
							key, err := common.String(eleKvs[i], nil)
							if err != nil {
								return nil, err
							}
							vv := eleKvs[i+1]
							switch key {
							case "id":
								cNode.Id, err = common.String(vv, nil)
							case "port":
								cNode.Port, err = common.Int(vv, nil)
							case "tls-port":
								cNode.TlsPort, err = common.Int(vv, nil)
							case "ip":
								cNode.Ip, err = common.String(vv, nil)
							case "endpoint":
								cNode.Endpoint, err = common.String(vv, nil)
							case "hostname":
								cNode.HostName, err = common.String(vv, nil)
							case "role":
								role, err := common.String(vv, nil)
								if err != nil {
									return nil, err
								}
								cNode.Role.Parse(role)
							case "replication-offset":
								cNode.ReplOffset, err = common.Int64(vv, nil)
							case "health":
								cNode.Health, err = common.String(vv, nil)
							}
							if err != nil {
								return nil, err
							}
						}
						var ep string
						if cNode.Ip != "" && cNode.Ip != "?" {
							ep = cNode.Ip
						} else if cNode.Endpoint != "" && cNode.Endpoint != "?" {
							ep = cNode.Endpoint
						} else {
							ep = cNode.HostName
						}
						cNode.Address = fmt.Sprintf("%s:%d", ep, cNode.Port)

						if cNode.Role == config.RedisRoleMaster {
							cShard.Master = cNode
						} else {
							cShard.Slaves = append(cShard.Slaves, cNode)
						}
					}
				}
			}
		}

		cShards = append(cShards, cShard)
	}

	return cShards, nil
}

func FixTopology(redisCfg *config.RedisConfig) error {
	//inputMode := config.InputModeStatic
	// if config.Get().Cluster != nil {
	// 	inputMode = config.Get().Input.Mode
	// }

	if redisCfg.Type == config.RedisTypeCluster {
		cli, err := client.NewRedis(*redisCfg)
		defer func() { log.LogIfError(cli.Close(), "close redis conn") }()

		// fix addresses
		if err != nil {
			return fmt.Errorf("new redis error : addr(%s), error(%w)", redisCfg.Address(), err)
		}
		masters, slaves, err := GetAllClusterAddress(cli)
		if err != nil {
			return err
		}
		redisCfg.SetMasterSlaves(masters, slaves)
		// if inputMode != config.InputModeStatic {
		// 	redisCfg.Addresses = masters
		// 	redisCfg.Addresses = append(redisCfg.Addresses, slaves...)
		// }

		// fix slots
		slots, err := GetClusterSlotDistribution(cli)
		if err != nil {
			return err
		}
		redisCfg.SetSlots(slots)

		// fix shards
		shards, err := GetAllClusterShard(cli)
		if err != nil {
			return err
		}
		redisCfg.SetClusterShards(shards)

		// migration
		migrating, err := GetClusterIsMigrating(cli)
		if err != nil {
			return err
		}
		redisCfg.SetMigrating(migrating)
	} else if redisCfg.Type == config.RedisTypeSentinel {
		// @TODO
		return fmt.Errorf("unknown redis type : %v, %s", redisCfg.Type, redisCfg.Address())
	} else if redisCfg.Type == config.RedisTypeStandalone {
		slots := map[string]*config.RedisSlots{}
		for _, addr := range redisCfg.Addresses {
			slots[addr] = &config.RedisSlots{
				Ranges: []config.RedisSlotRange{
					{
						Left:  0,
						Right: 16383,
					},
				},
			}
		}
		redisCfg.SetSlots(slots)
		return nil
	} else {
		return fmt.Errorf("unknown redis type : %v, %s", redisCfg.Type, redisCfg.Address())
	}
	return nil
}

func GetRunIds(cli client.Redis) (string, string, error) {
	str, err := common.String(cli.Do("info", "replication"))
	if err != nil {
		return "", "", err
	}

	lines := strings.Split(str, "\r\n")
	var id1 string
	var id2 string
	for _, line := range lines {
		af, ok := strings.CutPrefix(line, "master_replid:")
		if ok {
			id1 = af
		}
		af2, ok2 := strings.CutPrefix(line, "master_replid2:")
		if ok2 {
			id2 = af2
		}
	}
	return id1, id2, nil
}

func HGetAll(cli client.Redis, key string) ([]string, error) {
	kvs, err := common.Strings(cli.Do("hgetall", key))
	return kvs, err
}

func HGet(cli client.Redis, key string, field string) (string, error) {
	val, err := common.String(cli.Do("hget", key, field))
	return val, err
}

func HDel(cli client.Redis, key string, fileds ...string) error {
	if len(fileds) == 1 {
		_, err := cli.Do("hdel", key, fileds[0])
		return err
	}
	args := []interface{}{key}
	for _, f := range fileds {
		args = append(args, f)
	}
	_, err := cli.Do("hdel", args...)
	return err
}

func HSet(cli client.Redis, key string, pairs ...interface{}) error {
	// @TODO optimization : avoid copy
	args := []interface{}{key}
	args = append(args, pairs...)
	_, err := cli.Do("hset", args...)
	return err
}