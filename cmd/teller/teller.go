// Skycoin teller, which provides service of monitoring the bitcoin deposite
// and sending skycoin coins
package main

import (
	"errors"
	"io/ioutil"
	"log"
	"os/signal"
	"os/user"
	"sync"
	"time"

	"path/filepath"

	"github.com/boltdb/bolt"
	"github.com/google/gops/agent"
	"github.com/skycoin/skycoin/src/cipher"
	"github.com/skycoin/teller/src/daemon"
	"github.com/skycoin/teller/src/logger"
	"github.com/skycoin/teller/src/service"
	"github.com/skycoin/teller/src/service/btcaddrs"
	"github.com/skycoin/teller/src/service/cli"
	"github.com/skycoin/teller/src/service/config"
	"github.com/skycoin/teller/src/service/scanner"

	"fmt"
	"os"

	"bytes"

	"flag"

	"github.com/btcsuite/btcrpcclient"
	"github.com/skycoin/teller/src/service/exchange"
	"github.com/skycoin/teller/src/service/sender"
)

const (
	appDir = ".skycoin-teller"
	dbName = "data.db"
)

type dummyBtcScanner struct{}

func (s *dummyBtcScanner) AddDepositAddress(addr string) error {
	log.Println("dummyBtcScanner.AddDepositAddress", addr)
	return nil
}

func (s *dummyBtcScanner) GetDepositValue() <-chan scanner.DepositValue {
	log.Println("dummyBtcScanner.GetDepositValue")
	c := make(chan scanner.DepositValue)
	close(c)
	return c
}

type dummySkySender struct{}

func (s *dummySkySender) Send(destAddr string, coins int64, opt *sender.SendOption) (string, error) {
	log.Println("dummySkySender.Send", destAddr, coins, opt)
	return "", errors.New("dummy sky sender")
}

func (s *dummySkySender) SendAsync(destAddr string, coins int64, opt *sender.SendOption) (<-chan interface{}, error) {
	log.Println("dummySkySender.Send", destAddr, coins, opt)
	return nil, errors.New("dummy sky sender")
}

func (s *dummySkySender) IsClosed() bool {
	return true
}

func main() {
	configFile := flag.String("cfg", "config.json", "config.json file")
	btcAddrs := flag.String("btc-addrs", "btc_addresses.json", "btc_addresses.json file")
	proxyPubkey := flag.String("proxy-pubkey", "", "proxy pubkey")
	debug := flag.Bool("debug", false, "debug mode will show more detail logs")
	dummyMode := flag.Bool("dummy", false, "run without real btcd or skyd service")

	flag.Parse()

	// init logger
	log := logger.NewLogger("", *debug)

	if *proxyPubkey == "" {
		log.Println("-proxy-pubkey missing")
		return
	}

	rpubkey, err := cipher.PubKeyFromHex(*proxyPubkey)
	if err != nil {
		log.Println("Invalid proxy pubkey:", err)
		return
	}

	// generate local private key
	_, lseckey := cipher.GenerateKeyPair()

	auth := &daemon.Auth{
		RPubkey: rpubkey,
		LSeckey: lseckey,
	}

	// start gops agent, for profilling
	if err := agent.Listen(&agent.Options{
		NoShutdownCleanup: true,
	}); err != nil {
		log.Println("Start profile agent failed:", err)
		return
	}

	quit := make(chan struct{})
	go catchInterrupt(quit)

	// load config
	cfg, err := config.New(*configFile)
	if err != nil {
		log.Println("Load config failed:", err)
		return
	}

	appDir, err := createAppDirIfNotExist(appDir)
	if err != nil {
		log.Println("Create AppDir failed:", err)
		return
	}

	// open db
	dbPath := filepath.Join(appDir, dbName)
	db, err := bolt.Open(dbPath, 0700, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		log.Printf("Open db failed, err: %v\n", err)
		return
	}

	errC := make(chan error, 10)
	wg := sync.WaitGroup{}

	var scanServ *scanner.ScanService
	var scanCli exchange.BtcScanner
	var sendServ *sender.SendService
	var sendCli exchange.SkySender

	if *dummyMode {
		log.Println("btcd and skyd disabled, running in dummy mode")
		scanCli = &dummyBtcScanner{}
		sendCli = &dummySkySender{}
	} else {
		// create btc rpc client
		btcrpcConnConf := makeBtcrpcConfg(*cfg)
		btcrpc, err := btcrpcclient.New(&btcrpcConnConf, nil)
		if err != nil {
			log.Printf("Connect btcd failed: %v\n", err)
			return
		}

		log.Println("Connect to btcd success")

		// create scan service
		scanConfig := makeScanConfig(*cfg)
		scanServ, err = scanner.NewService(scanConfig, db, log, btcrpc)
		if err != nil {
			log.Println("Open scan service failed:", err)
			return
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			errC <- scanServ.Run()
		}()

		scanCli = scanner.NewScanner(scanServ)

		skyCli := cli.New(cfg.Skynode.WalletPath, cfg.Skynode.RPCAddress)

		// create skycoin send service
		sendServ = sender.NewService(makeSendConfig(*cfg), log, skyCli)

		wg.Add(1)
		go func() {
			defer wg.Done()
			errC <- sendServ.Run()
		}()

		sendCli = sender.NewSender(sendServ)
	}

	// create exchange service
	exchangeServ := exchange.NewService(makeExchangeConfig(*cfg), db, log, scanCli, sendCli)
	wg.Add(1)
	go func() {
		defer wg.Done()
		errC <- exchangeServ.Run()
	}()

	excCli := exchange.NewClient(exchangeServ)

	// create bitcoin address manager
	f, err := ioutil.ReadFile(*btcAddrs)
	if err != nil {
		log.Println("Load deposit bitcoin address list failed:", err)
		return
	}

	btcAddrGen, err := btcaddrs.New(db, bytes.NewReader(f), log)
	if err != nil {
		log.Println("Create bitcoin deposit address manager failed:", err)
		return
	}

	srv := service.New(makeServiceConfig(*cfg), auth, log, excCli, btcAddrGen)

	// Run the service
	wg.Add(1)
	go func() {
		defer wg.Done()
		errC <- srv.Run()
	}()

	select {
	case <-quit:
	case err := <-errC:
		if err != nil {
			log.Println(err)
		}
	}

	log.Println("Shutting down...")

	// close the scan service
	if scanServ != nil {
		scanServ.Shutdown()
	}

	// close the skycoin send service
	if sendServ != nil {
		sendServ.Shutdown()
	}

	// close exchange service
	exchangeServ.Shutdown()

	// close the teller service
	srv.Shutdown()
	wg.Wait()
	log.Println("Shutdown complete")
}

func makeServiceConfig(cfg config.Config) service.Config {
	return service.Config{
		ProxyAddr: cfg.ProxyAddress,

		ReconnectTime: cfg.ReconnectTime,
		PingTimeout:   cfg.PingTimeout,
		PongTimeout:   cfg.PongTimeout,
		DialTimeout:   cfg.DialTimeout,

		MaxBind: cfg.MaxBind,
	}
}

func makeExchangeConfig(cfg config.Config) exchange.Config {
	return exchange.Config{
		Rate: cfg.ExchangeRate,
	}
}

func makeScanConfig(cfg config.Config) scanner.Config {
	return scanner.Config{
		ScanPeriod:        cfg.Btcscan.CheckPeriod,
		DepositBuffersize: cfg.Btcscan.DepositBufferSize,
	}
}

func makeBtcrpcConfg(cfg config.Config) btcrpcclient.ConnConfig {
	certs, err := ioutil.ReadFile(cfg.Btcrpc.Cert)
	if err != nil {
		panic(fmt.Sprintf("btc rpc cert file does not exist in %s", cfg.Btcrpc.Cert))
	}

	return btcrpcclient.ConnConfig{
		Endpoint:     "ws",
		Host:         cfg.Btcrpc.Server,
		User:         cfg.Btcrpc.User,
		Pass:         cfg.Btcrpc.Pass,
		Certificates: certs,
	}
}

func makeSendConfig(cfg config.Config) sender.Config {
	return sender.Config{
		ReqBufSize: cfg.SkySender.ReqBuffSize,
	}
}

func createAppDirIfNotExist(app string) (string, error) {
	cur, err := user.Current()
	if err != nil {
		return "", err
	}
	path := filepath.Join(cur.HomeDir, app)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		// create the dir
		if err := os.Mkdir(path, 0700); err != nil {
			return "", err
		}
	}
	return path, nil
}

func catchInterrupt(quit chan<- struct{}) {
	sigchan := make(chan os.Signal, 1)
	signal.Notify(sigchan, os.Interrupt)
	<-sigchan
	signal.Stop(sigchan)
	close(quit)
}
