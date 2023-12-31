package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	_ "go.uber.org/automaxprocs"

	"github.com/ikenchina/redis-GunYu/cmd"
	"github.com/ikenchina/redis-GunYu/config"
	"github.com/ikenchina/redis-GunYu/pkg/log"
	"github.com/ikenchina/redis-GunYu/pkg/redis"
	"github.com/ikenchina/redis-GunYu/pkg/redis/client"
	usync "github.com/ikenchina/redis-GunYu/pkg/sync"
)

func main() {
	panicIfError(config.LoadFlags())
	panicIfError(runCmd())
}

func runCmd() error {
	var cmder cmd.Cmd
	switch config.GetFlag().Cmd {
	case "sync":
		panicIfError(config.InitConfig(config.GetFlag().ConfigPath))
		panicIfError(log.InitLog(*config.Get().Log))
		panicIfError(fixConfig())
		cmder = cmd.NewSyncerCmd()
	case "rdb":
		cmder = cmd.NewRdbCmd()
	default:
		panicIfError(fmt.Errorf("does not support command(%s)", config.GetFlag().Cmd))
	}

	usync.SafeGo(func() {
		handleSignal(cmder)
	}, nil)

	return cmder.Run()
}

func handleSignal(c cmd.Cmd) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGPIPE, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGABRT)
	for {
		sig := <-signals
		log.Infof("received signal: %s", sig)
		switch sig {
		case syscall.SIGPIPE:
		default:
			err := c.Stop()
			if err != nil {
				log.Errorf("cmd(%s) stopped with error : %v", c.Name(), err)
			} else {
				log.Infof("cmd(%s) is stopped", c.Name())
			}
			log.Sync()
			os.Exit(0)
			return
		}
	}
}

func panicIfError(err error) {
	if err == nil {
		return
	}
	log.Panic(err)
}

func fixConfig() (err error) {

	// redis version
	fixVersion := func(redisCfg *config.RedisConfig) error {
		if redisCfg.Version != "" {
			return nil
		}
		rr := redisCfg.SelNodes(true, config.SelNodeStrategyMaster)
		for _, svr := range rr {
			cli, err := client.NewRedis(svr)
			if err != nil {
				log.Errorf("new redis error : addr(%s), error(%v)", svr.Address(), err)
				continue
			}

			ver, err := redis.GetRedisVersion(cli)
			cli.Close()

			if err != nil {
				log.Errorf("redis get version error : addr(%s), error(%v)", svr.Address(), err)
				continue
			}
			redisCfg.Version = ver
			break
		}
		if redisCfg.Version == "" {
			return errors.New("cannot get redis version")
		}
		return nil
	}

	// addresses
	if err = redis.FixTopology(config.Get().Input.Redis); err != nil {
		return
	}
	if err = redis.FixTopology(config.Get().Output.Redis); err != nil {
		return
	}

	err = fixVersion(config.Get().Output.Redis)
	if err != nil {
		return
	}
	err = fixVersion(config.Get().Input.Redis)
	if err != nil {
		return
	}

	// fix concurrency

	return nil
}