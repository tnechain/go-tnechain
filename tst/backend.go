// Copyright 2014 The go-ethereum Authors
// This file is part of the go-tstereum library.
//
// The go-tstereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-tstereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-tstereum library. If not, see <http://www.gnu.org/licenses/>.

// Package tst implements the tnechain protocol.
package tst

import (
	"errors"
	"fmt"
	"math/big"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/tnechain/go-tnechain/accounts"
	"github.com/tnechain/go-tnechain/common"
	"github.com/tnechain/go-tnechain/common/hexutil"
	"github.com/tnechain/go-tnechain/consensus"
	"github.com/tnechain/go-tnechain/consensus/clique"
	"github.com/tnechain/go-tnechain/consensus/ethash"
	"github.com/tnechain/go-tnechain/core"
	"github.com/tnechain/go-tnechain/core/bloombits"
	"github.com/tnechain/go-tnechain/core/types"
	"github.com/tnechain/go-tnechain/core/vm"
	"github.com/tnechain/go-tnechain/tst/downloader"
	"github.com/tnechain/go-tnechain/tst/filters"
	"github.com/tnechain/go-tnechain/tst/gasprice"
	"github.com/tnechain/go-tnechain/tstdb"
	"github.com/tnechain/go-tnechain/event"
	"github.com/tnechain/go-tnechain/internal/ethapi"
	"github.com/tnechain/go-tnechain/log"
	"github.com/tnechain/go-tnechain/miner"
	"github.com/tnechain/go-tnechain/node"
	"github.com/tnechain/go-tnechain/p2p"
	"github.com/tnechain/go-tnechain/params"
	"github.com/tnechain/go-tnechain/rlp"
	"github.com/tnechain/go-tnechain/rpc"
)

type LesServer interface {
	Start(srvr *p2p.Server)
	Stop()
	Protocols() []p2p.Protocol
	SetBloomBitsIndexer(bbIndexer *core.ChainIndexer)
}

// tnechain implements the tnechain full node service.
type tnechain struct {
	config      *Config
	chainConfig *params.ChainConfig

	// Channel for shutting down the service
	shutdownChan  chan bool    // Channel for shutting down the tstereum
	stopDbUpgrade func() error // stop chain db sequential key upgrade

	// Handlers
	txPool          *core.TxPool
	blockchain      *core.BlockChain
	protocolManager *ProtocolManager
	lesServer       LesServer

	// DB interfaces
	chainDb tstdb.Database // Block chain database

	eventMux       *event.TypeMux
	engine         consensus.Engine
	accountManager *accounts.Manager

	bloomRequests chan chan *bloombits.Retrieval // Channel receiving bloom data retrieval requests
	bloomIndexer  *core.ChainIndexer             // Bloom indexer operating during block imports

	ApiBackend *TstApiBackend

	miner     *miner.Miner
	gasPrice  *big.Int
	tsterbase common.Address

	networkId     uint64
	netRPCService *ethapi.PublicNetAPI

	lock sync.RWMutex // Protects the variadic fields (e.g. gas price and tsterbase)
}

func (s *tnechain) AddLesServer(ls LesServer) {
	s.lesServer = ls
	ls.SetBloomBitsIndexer(s.bloomIndexer)
}

// New creates a new tnechain object (including the
// initialisation of the common tnechain object)
func New(ctx *node.ServiceContext, config *Config) (*tnechain, error) {
	if config.SyncMode == downloader.LightSync {
		return nil, errors.New("can't run tst.tnechain in light sync mode, use les.Lighttnechain")
	}
	if !config.SyncMode.IsValid() {
		return nil, fmt.Errorf("invalid sync mode %d", config.SyncMode)
	}
	chainDb, err := CreateDB(ctx, config, "chaindata")
	if err != nil {
		return nil, err
	}
	stopDbUpgrade := upgradeDeduplicateData(chainDb)
	chainConfig, genesisHash, genesisErr := core.SetupGenesisBlock(chainDb, config.Genesis)
	if _, ok := genesisErr.(*params.ConfigCompatError); genesisErr != nil && !ok {
		return nil, genesisErr
	}
	log.Info("Initialised chain configuration", "config", chainConfig)

	tst := &tnechain{
		config:         config,
		chainDb:        chainDb,
		chainConfig:    chainConfig,
		eventMux:       ctx.EventMux,
		accountManager: ctx.AccountManager,
		engine:         CreateConsensusEngine(ctx, &config.Tstash, chainConfig, chainDb),
		shutdownChan:   make(chan bool),
		stopDbUpgrade:  stopDbUpgrade,
		networkId:      config.NetworkId,
		gasPrice:       config.GasPrice,
		tsterbase:      config.Tsterbase,
		bloomRequests:  make(chan chan *bloombits.Retrieval),
		bloomIndexer:   NewBloomIndexer(chainDb, params.BloomBitsBlocks),
	}

	log.Info("Initialising tnechain protocol", "versions", ProtocolVersions, "network", config.NetworkId)

	if !config.SkipBcVersionCheck {
		bcVersion := core.GetBlockChainVersion(chainDb)
		if bcVersion != core.BlockChainVersion && bcVersion != 0 {
			return nil, fmt.Errorf("Blockchain DB version mismatch (%d / %d). Run gtst upgradedb.\n", bcVersion, core.BlockChainVersion)
		}
		core.WriteBlockChainVersion(chainDb, core.BlockChainVersion)
	}
	var (
		vmConfig    = vm.Config{EnablePreimageRecording: config.EnablePreimageRecording}
		cacheConfig = &core.CacheConfig{Disabled: config.NoPruning, TrieNodeLimit: config.TrieCache, TrieTimeLimit: config.TrieTimeout}
	)
	tst.blockchain, err = core.NewBlockChain(chainDb, cacheConfig, tst.chainConfig, tst.engine, vmConfig)
	if err != nil {
		return nil, err
	}
	// Rewind the chain in case of an incompatible config upgrade.
	if compat, ok := genesisErr.(*params.ConfigCompatError); ok {
		log.Warn("Rewinding chain to upgrade configuration", "err", compat)
		tst.blockchain.SetHead(compat.RewindTo)
		core.WriteChainConfig(chainDb, genesisHash, chainConfig)
	}
	tst.bloomIndexer.Start(tst.blockchain)

	if config.TxPool.Journal != "" {
		config.TxPool.Journal = ctx.ResolvePath(config.TxPool.Journal)
	}
	tst.txPool = core.NewTxPool(config.TxPool, tst.chainConfig, tst.blockchain)

	if tst.protocolManager, err = NewProtocolManager(tst.chainConfig, config.SyncMode, config.NetworkId, tst.eventMux, tst.txPool, tst.engine, tst.blockchain, chainDb); err != nil {
		return nil, err
	}
	tst.miner = miner.New(tst, tst.chainConfig, tst.EventMux(), tst.engine)
	tst.miner.SetExtra(makeExtraData(config.ExtraData))

	tst.ApiBackend = &TstApiBackend{tst, nil}
	gpoParams := config.GPO
	if gpoParams.Default == nil {
		gpoParams.Default = config.GasPrice
	}
	tst.ApiBackend.gpo = gasprice.NewOracle(tst.ApiBackend, gpoParams)

	return tst, nil
}

func makeExtraData(extra []byte) []byte {
	if len(extra) == 0 {
		// create default extradata
		extra, _ = rlp.EncodeToBytes([]interface{}{
			uint(params.VersionMajor<<16 | params.VersionMinor<<8 | params.VersionPatch),
			"gtst",
			runtime.Version(),
			runtime.GOOS,
		})
	}
	if uint64(len(extra)) > params.MaximumExtraDataSize {
		log.Warn("Miner extra data exceed limit", "extra", hexutil.Bytes(extra), "limit", params.MaximumExtraDataSize)
		extra = nil
	}
	return extra
}

// CreateDB creates the chain database.
func CreateDB(ctx *node.ServiceContext, config *Config, name string) (tstdb.Database, error) {
	db, err := ctx.OpenDatabase(name, config.DatabaseCache, config.DatabaseHandles)
	if err != nil {
		return nil, err
	}
	if db, ok := db.(*tstdb.LDBDatabase); ok {
		db.Meter("tst/db/chaindata/")
	}
	return db, nil
}

// CreateConsensusEngine creates the required type of consensus engine instance for an tnechain service
func CreateConsensusEngine(ctx *node.ServiceContext, config *ethash.Config, chainConfig *params.ChainConfig, db tstdb.Database) consensus.Engine {
	// If proof-of-authority is requested, set it up
	if chainConfig.Clique != nil {
		return clique.New(chainConfig.Clique, db)
	}
	// Otherwise assume proof-of-work
	switch {
	case config.PowMode == ethash.ModeFake:
		log.Warn("Tstash used in fake mode")
		return ethash.NewFaker()
	case config.PowMode == ethash.ModeTest:
		log.Warn("Tstash used in test mode")
		return ethash.NewTester()
	case config.PowMode == ethash.ModeShared:
		log.Warn("Tstash used in shared mode")
		return ethash.NewShared()
	default:
		engine := ethash.New(ethash.Config{
			CacheDir:       ctx.ResolvePath(config.CacheDir),
			CachesInMem:    config.CachesInMem,
			CachesOnDisk:   config.CachesOnDisk,
			DatasetDir:     config.DatasetDir,
			DatasetsInMem:  config.DatasetsInMem,
			DatasetsOnDisk: config.DatasetsOnDisk,
		})
		engine.SetThreads(-1) // Disable CPU mining
		return engine
	}
}

// APIs returns the collection of RPC services the tstereum package offers.
// NOTE, some of these services probably need to be moved to somewhere else.
func (s *tnechain) APIs() []rpc.API {
	apis := ethapi.GetAPIs(s.ApiBackend)

	// Append any APIs exposed explicitly by the consensus engine
	apis = append(apis, s.engine.APIs(s.BlockChain())...)

	// Append all the local APIs and return
	return append(apis, []rpc.API{
		{
			Namespace: "tst",
			Version:   "1.0",
			Service:   NewPublictnechainAPI(s),
			Public:    true,
		}, {
			Namespace: "tst",
			Version:   "1.0",
			Service:   NewPublicMinerAPI(s),
			Public:    true,
		}, {
			Namespace: "tst",
			Version:   "1.0",
			Service:   downloader.NewPublicDownloaderAPI(s.protocolManager.downloader, s.eventMux),
			Public:    true,
		}, {
			Namespace: "miner",
			Version:   "1.0",
			Service:   NewPrivateMinerAPI(s),
			Public:    false,
		}, {
			Namespace: "tst",
			Version:   "1.0",
			Service:   filters.NewPublicFilterAPI(s.ApiBackend, false),
			Public:    true,
		}, {
			Namespace: "admin",
			Version:   "1.0",
			Service:   NewPrivateAdminAPI(s),
		}, {
			Namespace: "debug",
			Version:   "1.0",
			Service:   NewPublicDebugAPI(s),
			Public:    true,
		}, {
			Namespace: "debug",
			Version:   "1.0",
			Service:   NewPrivateDebugAPI(s.chainConfig, s),
		}, {
			Namespace: "net",
			Version:   "1.0",
			Service:   s.netRPCService,
			Public:    true,
		},
	}...)
}

func (s *tnechain) ResetWithGenesisBlock(gb *types.Block) {
	s.blockchain.ResetWithGenesisBlock(gb)
}

func (s *tnechain) Tsterbase() (eb common.Address, err error) {
	s.lock.RLock()
	tsterbase := s.tsterbase
	s.lock.RUnlock()

	if tsterbase != (common.Address{}) {
		return tsterbase, nil
	}
	if wallets := s.AccountManager().Wallets(); len(wallets) > 0 {
		if accounts := wallets[0].Accounts(); len(accounts) > 0 {
			tsterbase := accounts[0].Address

			s.lock.Lock()
			s.tsterbase = tsterbase
			s.lock.Unlock()

			log.Info("Tsterbase automatically configured", "address", tsterbase)
			return tsterbase, nil
		}
	}
	return common.Address{}, fmt.Errorf("tsterbase must be explicitly specified")
}

// set in js console via admin interface or wrapper from cli flags
func (self *tnechain) SetTsterbase(tsterbase common.Address) {
	self.lock.Lock()
	self.tsterbase = tsterbase
	self.lock.Unlock()

	self.miner.SetTsterbase(tsterbase)
}

func (s *tnechain) StartMining(local bool) error {
	eb, err := s.Tsterbase()
	if err != nil {
		log.Error("Cannot start mining without tsterbase", "err", err)
		return fmt.Errorf("tsterbase missing: %v", err)
	}
	if clique, ok := s.engine.(*clique.Clique); ok {
		wallet, err := s.accountManager.Find(accounts.Account{Address: eb})
		if wallet == nil || err != nil {
			log.Error("Tsterbase account unavailable locally", "err", err)
			return fmt.Errorf("signer missing: %v", err)
		}
		clique.Authorize(eb, wallet.SignHash)
	}
	if local {
		// If local (CPU) mining is started, we can disable the transaction rejection
		// mechanism introduced to speed sync times. CPU mining on mainnet is ludicrous
		// so noone will ever hit this path, whereas marking sync done on CPU mining
		// will ensure that private networks work in single miner mode too.
		atomic.StoreUint32(&s.protocolManager.acceptTxs, 1)
	}
	go s.miner.Start(eb)
	return nil
}

func (s *tnechain) StopMining()         { s.miner.Stop() }
func (s *tnechain) IsMining() bool      { return s.miner.Mining() }
func (s *tnechain) Miner() *miner.Miner { return s.miner }

func (s *tnechain) AccountManager() *accounts.Manager  { return s.accountManager }
func (s *tnechain) BlockChain() *core.BlockChain       { return s.blockchain }
func (s *tnechain) TxPool() *core.TxPool               { return s.txPool }
func (s *tnechain) EventMux() *event.TypeMux           { return s.eventMux }
func (s *tnechain) Engine() consensus.Engine           { return s.engine }
func (s *tnechain) ChainDb() tstdb.Database            { return s.chainDb }
func (s *tnechain) IsListening() bool                  { return true } // Always listening
func (s *tnechain) TstVersion() int                    { return int(s.protocolManager.SubProtocols[0].Version) }
func (s *tnechain) NetVersion() uint64                 { return s.networkId }
func (s *tnechain) Downloader() *downloader.Downloader { return s.protocolManager.downloader }

// Protocols implements node.Service, returning all the currently configured
// network protocols to start.
func (s *tnechain) Protocols() []p2p.Protocol {
	if s.lesServer == nil {
		return s.protocolManager.SubProtocols
	}
	return append(s.protocolManager.SubProtocols, s.lesServer.Protocols()...)
}

// Start implements node.Service, starting all internal goroutines needed by the
// tnechain protocol implementation.
func (s *tnechain) Start(srvr *p2p.Server) error {
	// Start the bloom bits servicing goroutines
	s.startBloomHandlers()

	// Start the RPC service
	s.netRPCService = ethapi.NewPublicNetAPI(srvr, s.NetVersion())

	// Figure out a max peers count based on the server limits
	maxPeers := srvr.MaxPeers
	if s.config.LightServ > 0 {
		if s.config.LightPeers >= srvr.MaxPeers {
			return fmt.Errorf("invalid peer config: light peer count (%d) >= total peer count (%d)", s.config.LightPeers, srvr.MaxPeers)
		}
		maxPeers -= s.config.LightPeers
	}
	// Start the networking layer and the light server if requested
	s.protocolManager.Start(maxPeers)
	if s.lesServer != nil {
		s.lesServer.Start(srvr)
	}
	return nil
}

// Stop implements node.Service, terminating all internal goroutines used by the
// tnechain protocol.
func (s *tnechain) Stop() error {
	if s.stopDbUpgrade != nil {
		s.stopDbUpgrade()
	}
	s.bloomIndexer.Close()
	s.blockchain.Stop()
	s.protocolManager.Stop()
	if s.lesServer != nil {
		s.lesServer.Stop()
	}
	s.txPool.Stop()
	s.miner.Stop()
	s.eventMux.Stop()

	s.chainDb.Close()
	close(s.shutdownChan)

	return nil
}
