package node

import (
	"context"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/event"
	"github.com/inconshreveable/log15"
	"github.com/vechain/thor/block"
	"github.com/vechain/thor/cache"
	"github.com/vechain/thor/chain"
	"github.com/vechain/thor/co"
	"github.com/vechain/thor/comm"
	"github.com/vechain/thor/consensus"
	"github.com/vechain/thor/logdb"
	"github.com/vechain/thor/packer"
	"github.com/vechain/thor/state"
	"github.com/vechain/thor/thor"
	"github.com/vechain/thor/tx"
	"github.com/vechain/thor/txpool"
)

var log = log15.New("pkg", "node")

type Node struct {
	goes   co.Goes
	packer *packer.Packer
	cons   *consensus.Consensus

	bestBlockFeed   event.Feed
	packedBlockFeed event.Feed
	blockChunkCh    chan []*block.Block
	blockChunkAckCh chan error

	master *Master
	chain  *chain.Chain
	logDB  *logdb.LogDB
	txPool *txpool.TxPool
	comm   *comm.Communicator
}

func New(
	master *Master,
	chain *chain.Chain,
	stateCreator *state.Creator,
	logDB *logdb.LogDB,
	txPool *txpool.TxPool,
	comm *comm.Communicator,
) *Node {
	return &Node{
		packer:          packer.New(chain, stateCreator, master.Address(), master.Beneficiary),
		cons:            consensus.New(chain, stateCreator),
		blockChunkCh:    make(chan []*block.Block),
		blockChunkAckCh: make(chan error),
		master:          master,
		chain:           chain,
		logDB:           logDB,
		txPool:          txPool,
		comm:            comm,
	}
}

func (n *Node) Run(ctx context.Context) error {
	defer n.goes.Wait()

	n.goes.Go(func() { n.txLoop(ctx) })
	n.goes.Go(func() { n.packerLoop(ctx) })
	n.goes.Go(func() { n.consensusLoop(ctx) })

	return nil
}
func (n *Node) HandleBlockChunk(chunk []*block.Block) error {
	n.blockChunkCh <- chunk
	return <-n.blockChunkAckCh
}

func (n *Node) waitForSynced(ctx context.Context) bool {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if n.comm.IsSynced() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

func (n *Node) packerLoop(ctx context.Context) {
	log.Debug("enter packer loop")
	defer log.Debug("leave packer loop")

	log.Info("waiting for synchronization...")
	// wait for synced
	if !n.waitForSynced(ctx) {
		return
	}

	log.Info("synchronization process done")

	parent, err := n.chain.GetBestBlock()
	if err != nil {
		log.Error("failed to get best block", "err", err)
		return
	}

	var scope event.SubscriptionScope
	bestBlockCh := make(chan *bestBlockEvent)
	scope.Track(n.bestBlockFeed.Subscribe(bestBlockCh))

	defer scope.Close()

	timer := time.NewTimer(0)
	defer timer.Stop()

	var prepareErrorLogged bool
	for {
		now := uint64(time.Now().Unix())
		if timestamp, adopt, pack, err := n.packer.Prepare(parent.Header(), now); err != nil {
			if !prepareErrorLogged {
				prepareErrorLogged = true
				log.Warn("unable to pack block", "err", err)
			}
		} else {
			timer.Reset(time.Duration(timestamp-now) * time.Second)
			select {
			case <-ctx.Done():
				return
			case bestBlock := <-bestBlockCh:
				parent = bestBlock.Block
				continue
			case <-timer.C:
				n.pack(adopt, pack)
			}
		}

		select {
		case <-ctx.Done():
			return
		case bestBlock := <-bestBlockCh:
			parent = bestBlock.Block
			continue
		}
	}
}

func (n *Node) consensusLoop(ctx context.Context) {
	log.Debug("enter consensus loop")
	defer log.Debug("leave consensus loop")

	var scope event.SubscriptionScope
	packedBlockCh := make(chan *packedBlockEvent)
	newBlockCh := make(chan *comm.NewBlockEvent)

	scope.Track(n.packedBlockFeed.Subscribe(packedBlockCh))
	scope.Track(n.comm.SubscribeBlock(newBlockCh))
	defer scope.Close()

	futureBlocks := cache.NewPrioCache(256)

	ticker := time.NewTicker(time.Duration(thor.BlockInterval) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case packedBlock := <-packedBlockCh:
			startTime := mclock.Now()
			if _, err := packedBlock.stage.Commit(); err != nil {
				log.Error("failed to commit state of packed block", "err", err)
				continue
			}
			isTrunk, err := n.insertBlock(packedBlock.Block, packedBlock.receipts)
			if err != nil {
				continue
			}
			commitElapsed := mclock.Now() - startTime

			if isTrunk {
				log.Info("📦 new block packed",
					"txs", len(packedBlock.receipts),
					"mgas", float64(packedBlock.Header().GasUsed())/1000/1000,
					"et", fmt.Sprintf("%v|%v", common.PrettyDuration(packedBlock.elapsed), common.PrettyDuration(commitElapsed)),
					"id", shortID(packedBlock.Header().ID()),
				)
			}
		case newBlock := <-newBlockCh:
			var stats blockStats
			if isTrunk, err := n.processBlock(newBlock.Block, &stats); err != nil {
				if consensus.IsFutureBlock(err) || consensus.IsParentMissing(err) {
					futureBlocks.Set(newBlock.Header().ID(), newBlock, -float64(newBlock.Header().Number()))
				}
			} else if isTrunk {
				log.Info("imported blocks", stats.LogContext(newBlock.Block.Header())...)
			}
		case blocks := <-n.blockChunkCh:
			n.blockChunkAckCh <- func() error {
				var stats blockStats
				startTime := mclock.Now()
				for i, block := range blocks {
					if _, err := n.processBlock(block, &stats); err != nil {
						return err
					}

					if stats.processed > 0 &&
						(i == len(blocks)-1 || mclock.Now()-startTime > mclock.AbsTime(5*time.Second)) {
						log.Info("imported blocks", stats.LogContext(block.Header())...)
						stats = blockStats{}
					}

					select {
					case <-ctx.Done():
						return ctx.Err()
					default:
					}
				}
				return nil
			}()
		}
	}
}

func (n *Node) pack(adopt packer.Adopt, pack packer.Pack) {
	startTime := mclock.Now()
	for _, tx := range n.txPool.Pending() {
		err := adopt(tx)
		switch {
		case packer.IsGasLimitReached(err):
			break
		case packer.IsTxNotAdoptableNow(err):
			continue
		default:
			n.txPool.Remove(tx.ID())
		}
	}
	newBlock, stage, receipts, err := pack(n.master.PrivateKey)
	if err != nil {
		log.Error("failed to pack block", "err", err)
		return
	}

	elapsed := mclock.Now() - startTime
	n.goes.Go(func() { n.packedBlockFeed.Send(&packedBlockEvent{newBlock, stage, receipts, elapsed}) })

	if elapsed > 0 {
		gasUsed := newBlock.Header().GasUsed()
		// calc target gas limit only if gas used above third of gas limit
		if gasUsed > newBlock.Header().GasLimit()/3 {
			targetGasLimit := uint64(thor.TolerableBlockPackingTime) * gasUsed / uint64(elapsed)
			n.packer.SetTargetGasLimit(targetGasLimit)
		}
	}
}

func (n *Node) processBlock(blk *block.Block, stats *blockStats) (bool, error) {
	startTime := mclock.Now()
	now := uint64(time.Now().Unix())
	stage, receipts, err := n.cons.Process(blk, now)
	if err != nil {
		switch {
		case consensus.IsKnownBlock(err):
			stats.UpdateIgnored(1)
			return false, nil
		case consensus.IsFutureBlock(err) || consensus.IsParentMissing(err):
			stats.UpdateQueued(1)
		case consensus.IsCritical(err):
			msg := fmt.Sprintf(`failed to process block due to consensus failure \n%v\n`, blk.Header())
			log.Error(msg, "err", err)
		default:
			log.Error("failed to process block", "err", err)
		}
		return false, err
	}

	execElapsed := mclock.Now() - startTime

	if _, err := stage.Commit(); err != nil {
		log.Error("failed to commit state", "err", err)
		return false, err
	}

	isTrunk, err := n.insertBlock(blk, receipts)
	if err != nil {
		log.Error("failed to insert block", "err", err)
		return false, err
	}
	commitElapsed := mclock.Now() - startTime - execElapsed
	stats.UpdateProcessed(1, len(receipts), execElapsed, commitElapsed, blk.Header().GasUsed())
	return isTrunk, err
}

func (n *Node) insertBlock(newBlock *block.Block, receipts tx.Receipts) (bool, error) {
	isTrunk, err := n.cons.IsTrunk(newBlock.Header())
	if err != nil {
		return false, err
	}

	fork, err := n.chain.AddBlock(newBlock, receipts, isTrunk)
	if err != nil {
		return false, err
	}

	forkIDs := make([]thor.Bytes32, 0, len(fork.Branch))
	for _, block := range fork.Branch {
		forkIDs = append(forkIDs, block.Header().ID())
		for _, tx := range block.Transactions() {
			n.txPool.Add(tx)
		}
	}

	batch := n.logDB.Prepare(newBlock.Header())
	for i, tx := range newBlock.Transactions() {
		origin, _ := tx.Signer()
		txBatch := batch.ForTransaction(tx.ID(), origin)
		receipt := receipts[i]

		for _, output := range receipt.Outputs {
			txBatch.Insert(output.Events, output.Transfers)
		}
	}

	if err := batch.Commit(forkIDs...); err != nil {
		return false, err
	}
	if isTrunk {
		n.goes.Go(func() {
			n.bestBlockFeed.Send(&bestBlockEvent{newBlock})
			n.comm.BroadcastBlock(newBlock)
		})
	}
	return isTrunk, nil
}

func (n *Node) txLoop(ctx context.Context) {
	log.Debug("enter tx loop")
	defer log.Debug("leave tx loop")

	txPoolCh := make(chan *tx.Transaction)
	commTxCh := make(chan *comm.NewTransactionEvent)
	var scope event.SubscriptionScope
	scope.Track(n.txPool.SubscribeNewTransaction(txPoolCh))
	scope.Track(n.comm.SubscribeTransaction(commTxCh))
	defer scope.Close()

	for {
		select {
		case <-ctx.Done():
			return
		case tx := <-txPoolCh:
			n.comm.BroadcastTx(tx)
		case tx := <-commTxCh:
			n.txPool.Add(tx.Transaction)
		}
	}
}