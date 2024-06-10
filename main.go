// https://github.com/jrick/btcsim/blob/master/btcwallet.go
package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/lightningnetwork/lnd/blockcache"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/chainntnfs/bitcoindnotify"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/kvdb"
)

var startBlock int32 = 1
var defaultBitcoindBlockCacheSize uint64 = 20 * 1024 * 1024 // 20 MB

func handlerChan(txpool chan<- struct{}, block chan chainhash.Hash) *rpcclient.NotificationHandlers {
	return &rpcclient.NotificationHandlers{
		// When a block higher than stopBlock connects to the chain,
		// send a signal to stop actors. This is used so main can break from
		// select and call actor.Stop to stop actors.
		OnBlockConnected: func(hash *chainhash.Hash, h int32, t time.Time) {
			fmt.Printf("\r%d/%d/%#v", h, startBlock, t)
			block <- *hash
		},

		// Send a signal that a tx has been accepted into the mempool. Based on
		// the tx curve, the receiver will need to wait until required no of tx
		// are filled up in the mempool
		OnTxAccepted: func(hash *chainhash.Hash, amount btcutil.Amount) {
			if txpool != nil {
				// this will not be blocked because we're creating only
				// required no of tx and receiving all of them
				txpool <- struct{}{}
			}
		},

		OnFilteredBlockConnected: func(height int32, header *wire.BlockHeader, txns []*btcutil.Tx) {
			log.Printf("Block connected: %v (%d) %v",
				header.BlockHash(), height, header.Timestamp)
		},
		OnFilteredBlockDisconnected: func(height int32, header *wire.BlockHeader) {
			log.Printf("Block disconnected: %v (%d) %v",
				header.BlockHash(), height, header.Timestamp)
		},
	}
}

func BuildDialer(rpcHost string) func(string) (net.Conn, error) {
	return func(addr string) (net.Conn, error) {
		return net.Dial("tcp", rpcHost)
	}
}

func main() {
	connCfg2 := &rpcclient.ConnConfig{
		Host:         os.Args[1],
		User:         os.Args[2],
		Pass:         os.Args[3],
		HTTPPostMode: true, // Bitcoin core only supports HTTP POST mode
		DisableTLS:   true, // Bitcoin core does not provide TLS by default
	}
	txpool := make(chan struct{}, 100)
	block := make(chan chainhash.Hash, 100)
	client, err := rpcclient.New(connCfg2, handlerChan(txpool, block))
	if err != nil {
		log.Fatalf("Init client, %#v ", err.Error())
	}
	defer client.Shutdown()
	log.Printf("Connected")

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

	blockHeight, _ := client.GetBlockCount()
	log.Printf("New notifier at %d\n", blockHeight)
	ev, err := chainNotifier.RegisterConfirmationsNtfn(
		txid, txn.MsgTx().TxOut[0].PkScript, 6, uint32(blockHeight),
	)

	log.Println("Wait for")
	waitForUnbondingTxConfirmation(ev)
	log.Println("Wait for shutdown...")
}

func waitForUnbondingTxConfirmation(
	waitEv *chainntnfs.ConfirmationEvent,
) {
	defer waitEv.Cancel()

	for {
		select {
		case conf := <-waitEv.Confirmed:
			log.Printf("conf: %#v\n", conf)
		case u := <-waitEv.Updates:
			log.Printf("Unbonding transaction received confirmation, %#v", u)
		}
	}
}
