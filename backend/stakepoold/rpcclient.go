package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"sync"
	"time"

	"github.com/Eacred/eacrd/chaincfg/chainhash"
	"github.com/Eacred/eacrd/rpcclient"
	"github.com/Eacred/eacrstakepool/backend/stakepoold/stakepool"
	"github.com/Eacred/eacrstakepool/backend/stakepoold/userdata"
)

var requiredChainServerAPI = semver{major: 6, minor: 1, patch: 1}
var requiredWalletAPI = semver{major: 6, minor: 2, patch: 0}

func connectNodeRPC(spd *stakepool.Stakepoold, cfg *config) (*rpcclient.Client, semver, error) {
	var nodeVer semver

	ecrdCert, err := ioutil.ReadFile(cfg.EcrdCert)
	if err != nil {
		log.Errorf("Failed to read ecrd cert file at %s: %s\n",
			cfg.EcrdCert, err.Error())
		return nil, nodeVer, err
	}

	log.Debugf("Attempting to connect to ecrd RPC %s as user %s "+
		"using certificate located in %s",
		cfg.EcrdHost, cfg.EcrdUser, cfg.EcrdCert)

	connCfgDaemon := &rpcclient.ConnConfig{
		Host:         cfg.EcrdHost,
		Endpoint:     "ws", // websocket
		User:         cfg.EcrdUser,
		Pass:         cfg.EcrdPassword,
		Certificates: ecrdCert,
	}

	ntfnHandlers := getNodeNtfnHandlers(spd)
	ecrdClient, err := rpcclient.New(connCfgDaemon, ntfnHandlers)
	if err != nil {
		log.Errorf("Failed to start ecrd RPC client: %s\n", err.Error())
		return nil, nodeVer, err
	}

	// Ensure the RPC server has a compatible API version.
	ver, err := ecrdClient.Version()
	if err != nil {
		log.Error("Unable to get RPC version: ", err)
		return nil, nodeVer, fmt.Errorf("Unable to get node RPC version")
	}

	ecrdVer := ver["ecrdjsonrpcapi"]
	nodeVer = semver{ecrdVer.Major, ecrdVer.Minor, ecrdVer.Patch}

	if !semverCompatible(requiredChainServerAPI, nodeVer) {
		return nil, nodeVer, fmt.Errorf("Node JSON-RPC server does not have "+
			"a compatible API version. Advertises %v but require %v",
			nodeVer, requiredChainServerAPI)
	}

	return ecrdClient, nodeVer, nil
}

func connectWalletRPC(ctx context.Context, wg *sync.WaitGroup, cfg *config) (*stakepool.Client, semver, error) {
	var walletVer semver

	dcrwCert, err := ioutil.ReadFile(cfg.WalletCert)
	if err != nil {
		log.Errorf("Failed to read eacrwallet cert file at %s: %s\n",
			cfg.WalletCert, err.Error())
		return nil, walletVer, err
	}

	log.Infof("Attempting to connect to eacrwallet RPC %s as user %s "+
		"using certificate located in %s",
		cfg.WalletHost, cfg.WalletUser, cfg.WalletCert)

	connCfgWallet := &rpcclient.ConnConfig{
		Host:                 cfg.WalletHost,
		Endpoint:             "ws",
		User:                 cfg.WalletUser,
		Pass:                 cfg.WalletPassword,
		Certificates:         dcrwCert,
		DisableAutoReconnect: true,
	}

	ntfnHandlers := getWalletNtfnHandlers()

	// New also starts an autoreconnect function.
	dcrwClient, err := stakepool.NewClient(ctx, wg, connCfgWallet, ntfnHandlers)
	if err != nil {
		log.Errorf("Verify that username and password is correct and that "+
			"rpc.cert is for your wallet: %v", cfg.WalletCert)
		return nil, walletVer, err
	}

	// Ensure the wallet RPC server has a compatible API version.
	ver, err := dcrwClient.RPCClient().Version()
	if err != nil {
		log.Error("Unable to get RPC version: ", err)
		return nil, walletVer, fmt.Errorf("Unable to get node RPC version")
	}

	dcrwVer := ver["eacrwalletjsonrpcapi"]
	walletVer = semver{dcrwVer.Major, dcrwVer.Minor, dcrwVer.Patch}

	if !semverCompatible(requiredWalletAPI, walletVer) {
		log.Errorf("Node JSON-RPC server %v does not have "+
			"a compatible API version. Advertizes %v but require %v",
			cfg.WalletHost, walletVer, requiredWalletAPI)
		return nil, walletVer, fmt.Errorf("Incompatible eacrwallet RPC version")
	}

	return dcrwClient, walletVer, nil
}

func walletGetTickets(spd *stakepool.Stakepoold) (map[chainhash.Hash]string, map[chainhash.Hash]string, error) {
	blockHashToHeightCache := make(map[chainhash.Hash]int32)

	// This is suboptimal to copy and needs fixing.
	userVotingConfig := make(map[string]userdata.UserVotingConfig)
	spd.RLock()
	for k, v := range spd.UserVotingConfig {
		userVotingConfig[k] = v
	}
	spd.RUnlock()

	ignoredLowFeeTickets := make(map[chainhash.Hash]string)
	liveTickets := make(map[chainhash.Hash]string)
	var normalFee int

	log.Info("Calling GetTickets...")
	timenow := time.Now()
	tickets, err := spd.WalletConnection.RPCClient().GetTickets(false)
	log.Infof("GetTickets: took %v", time.Since(timenow))

	if err != nil {
		log.Warnf("GetTickets failed: %v", err)
		return ignoredLowFeeTickets, liveTickets, err
	}

	type promise struct {
		rpcclient.FutureGetTransactionResult
	}
	promises := make([]promise, 0, len(tickets))

	log.Debugf("setting up GetTransactionAsync for %v tickets", len(tickets))
	for _, ticket := range tickets {
		// lookup ownership of each ticket
		promises = append(promises, promise{spd.WalletConnection.RPCClient().GetTransactionAsync(ticket)})
	}

	var counter int
	for _, p := range promises {
		counter++
		log.Debugf("Receiving GetTransaction result for ticket %v/%v", counter, len(tickets))
		gt, err := p.Receive()
		if err != nil {
			// All tickets should exist and be able to be looked up
			log.Warnf("GetTransaction error: %v", err)
			continue
		}
		for i := range gt.Details {
			addr := gt.Details[i].Address
			_, ok := userVotingConfig[addr]
			if !ok {
				log.Warnf("Could not map ticket %v to a user, user %v doesn't exist", gt.TxID, addr)
				continue
			}

			hash, err := chainhash.NewHashFromStr(gt.TxID)
			if err != nil {
				log.Warnf("invalid ticket %v", err)
				continue
			}

			// All tickets are present in the GetTickets response, whether they
			// pay the correct fee or not.  So we need to verify fees and
			// sort the tickets into their respective maps.
			_, isAdded := spd.AddedLowFeeTicketsMSA[*hash]
			if isAdded {
				liveTickets[*hash] = userVotingConfig[addr].MultiSigAddress
			} else {
				msgTx, err := stakepool.MsgTxFromHex(gt.Hex)
				if err != nil {
					log.Warnf("MsgTxFromHex failed for %v: %v", gt.Hex, err)
					continue
				}

				// look up the height at which this ticket was purchased
				var ticketBlockHeight int32
				ticketBlockHash, err := chainhash.NewHashFromStr(gt.BlockHash)
				if err != nil {
					log.Warnf("NewHashFromStr failed for %v: %v", gt.BlockHash, err)
					continue
				}

				height, inCache := blockHashToHeightCache[*ticketBlockHash]
				if inCache {
					ticketBlockHeight = height
				} else {
					gbh, err := spd.NodeConnection.GetBlockHeader(ticketBlockHash)
					if err != nil {
						log.Warnf("GetBlockHeader failed for %v: %v", ticketBlockHash, err)
						continue
					}

					blockHashToHeightCache[*ticketBlockHash] = int32(gbh.Height)
					ticketBlockHeight = int32(gbh.Height)
				}

				ticketFeesValid, err := spd.EvaluateStakePoolTicket(msgTx, ticketBlockHeight)

				if err != nil {
					log.Warnf("ignoring ticket %v for multisig %v due to error: %v",
						*hash, spd.UserVotingConfig[addr].MultiSigAddress, err)
					ignoredLowFeeTickets[*hash] = userVotingConfig[addr].MultiSigAddress
				} else if ticketFeesValid {
					normalFee++
					liveTickets[*hash] = userVotingConfig[addr].MultiSigAddress
				} else {
					log.Warnf("ignoring ticket %v for multisig %v due to invalid fee",
						*hash, spd.UserVotingConfig[addr].MultiSigAddress)
					ignoredLowFeeTickets[*hash] = userVotingConfig[addr].MultiSigAddress
				}
			}
			break
		}
	}

	log.Infof("tickets loaded -- addedLowFee %v ignoredLowFee %v normalFee %v "+
		"live %v total %v", len(spd.AddedLowFeeTicketsMSA),
		len(ignoredLowFeeTickets), normalFee, len(liveTickets),
		len(tickets))

	return ignoredLowFeeTickets, liveTickets, nil
}
