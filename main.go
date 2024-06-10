// https://github.com/jrick/btcsim/blob/master/btcwallet.go
package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/lightningnetwork/lnd/blockcache"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/chainntnfs/bitcoindnotify"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/kvdb"
)

var startBlock int32 = 1
var defaultBitcoindBlockCacheSize uint64 = 20 * 1024 * 1024 // 20 MB

func BuildDialer(rpcHost string) func(string) (net.Conn, error) {
	return func(addr string) (net.Conn, error) {
		return net.Dial("tcp", rpcHost)
	}
}

func main() {

	connCfg := &rpcclient.ConnConfig{
		Host:         os.Args[1],
		User:         os.Args[2],
		Pass:         os.Args[3],
		HTTPPostMode: true, // Bitcoin core only supports HTTP POST mode
		DisableTLS:   true, // Bitcoin core does not provide TLS by default
	}
	client, err := rpcclient.New(connCfg, nil)
	if err != nil {
		log.Fatalf("Init client, %#v ", err.Error())
	}
	defer client.Shutdown()
	log.Printf("Connected")

	/*
			wcr, err := client.CreateWallet("btcstaker3")
			if err != nil {
				log.Fatal(err)
			}
			log.Println(wcr)

		wallet, err := client.LoadWallet("btcstaker")
		if err != nil {
			log.Printf("Load wallet %#v\n", err)
		}
		log.Println(wallet)
	*/

	if err := client.WalletPassphrase("btcstaker", 30); err != nil {
		log.Fatalf("load wallet %#v\n", err)
	}

	boltConfig := kvdb.BoltBackendConfig{
		DBPath:            "/tmp/btcd-db",
		DBFileName:        "bolt",
		NoFreelistSync:    true,
		AutoCompact:       false,
		AutoCompactMinAge: kvdb.DefaultBoltAutoCompactMinAge,
		DBTimeout:         kvdb.DefaultDBTimeout,
	}
	db, err := kvdb.GetBoltBackend(&boltConfig)

	if err != nil {
		err = fmt.Errorf("failed to load db backend: %w", err)
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	hintCache, err := channeldb.NewHeightHintCache(
		channeldb.CacheConfig{
			// TODO: Investigate this option. Lighting docs mention that this is necessary for some edge case
			QueryDisable: false,
		}, db,
	)

	if err != nil {
		log.Fatalf("unable to create height hint cache: %v", err)
	}

	params := &chaincfg.RegressionNetParams

	txid, err := chainhash.NewHashFromStr(os.Args[4])
	if err != nil {
		log.Fatal(err)
	}

	// get transaction
	txn, err := client.GetRawTransaction(txid)
	if err != nil {
		log.Fatal(err)
	}

	bitcoindCfg := &chain.BitcoindConfig{
		ChainParams:        params,
		Host:               os.Args[1],
		User:               os.Args[2],
		Pass:               os.Args[3],
		Dialer:             BuildDialer(os.Args[1]),
		PrunedModeMaxPeers: 10,
		PollingConfig: &chain.PollingConfig{
			BlockPollingInterval:    time.Duration(1) * time.Second,
			TxPollingInterval:       time.Duration(1) * time.Second,
			TxPollingIntervalJitter: 0.5,
		},
	}
	bitcoindConn, err := chain.NewBitcoindConn(bitcoindCfg)
	if err != nil {
		log.Fatalf("New conn %s", err.Error())
	}

	if err := bitcoindConn.Start(); err != nil {
		log.Fatalf("unable to connect to "+
			"bitcoind: %v", err)
	}
	log.Println("Started")

	chainNotifier := bitcoindnotify.New(
		bitcoindConn, params, hintCache,
		hintCache, blockcache.NewBlockCache(defaultBitcoindBlockCacheSize),
	)
	if err != nil {
		log.Fatal(err)
	}

	if err := chainNotifier.Start(); err != nil {
		log.Fatalf("start notifier, %#v\n", err)
	}

	blockHeight, _ := client.GetBlockCount()
	log.Printf("New notifier at %d\n", blockHeight)
	ev, err := chainNotifier.RegisterConfirmationsNtfn(
		txid, txn.MsgTx().TxOut[0].PkScript, 6, uint32(blockHeight),
	)

	waitForTxConfirmation(ev)
}

func waitForTxConfirmation(
	waitEv *chainntnfs.ConfirmationEvent,
) {
	defer waitEv.Cancel()

	for {
		select {
		case conf := <-waitEv.Confirmed:
			log.Printf("conf: %#v\n", conf)
			return
		case u := <-waitEv.Updates:
			log.Printf("Received pre-confirmation, %#v", u)
		}
	}
}
