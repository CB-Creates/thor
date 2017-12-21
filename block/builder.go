package block

import (
	"math/big"

	"github.com/vechain/thor/acc"
	"github.com/vechain/thor/cry"
	"github.com/vechain/thor/tx"
)

// Builder to make it easy to build a block object.
type Builder struct {
	header headerContent
	txs    tx.Transactions
}

// ParentHash set parent hash.
func (b *Builder) ParentHash(hash cry.Hash) *Builder {
	b.header.ParentHash = hash
	return b
}

// Timestamp set timestamp.
func (b *Builder) Timestamp(ts uint64) *Builder {
	b.header.Timestamp = ts
	return b
}

// TotalScore set total score.
func (b *Builder) TotalScore(s *big.Int) *Builder {
	b.header.TotalScore = new(big.Int).Set(s)
	return b
}

// GasLimit set gas limit.
func (b *Builder) GasLimit(limit *big.Int) *Builder {
	b.header.GasLimit = new(big.Int).Set(limit)
	return b
}

// GasUsed set gas used.
func (b *Builder) GasUsed(used *big.Int) *Builder {
	b.header.GasUsed = new(big.Int).Set(used)
	return b
}

// Beneficiary set recipient of reward.
func (b *Builder) Beneficiary(addr acc.Address) *Builder {
	b.header.Beneficiary = addr
	return b
}

// StateRoot set state root.
func (b *Builder) StateRoot(hash cry.Hash) *Builder {
	b.header.StateRoot = hash
	return b
}

// ReceiptsRoot set receipts root.
func (b *Builder) ReceiptsRoot(hash cry.Hash) *Builder {
	b.header.ReceiptsRoot = hash
	return b
}

// Transaction add a transaction.
func (b *Builder) Transaction(tx *tx.Transaction) *Builder {
	b.txs = append(b.txs, tx)
	return b
}

// Build build a block object.
func (b *Builder) Build() *Block {
	header := b.header
	header.TxsRoot = b.txs.RootHash()

	return &Block{
		&Header{content: header},
		b.txs.Copy(),
	}
}
