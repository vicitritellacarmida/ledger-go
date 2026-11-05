package service

import (
	"context"

	"github.com/vicitritellacarmida/ledger-go/internal/domain"
)

const auditBatchSize = 500

// ChainReport is the result of verifying the audit hash chain.
type ChainReport struct {
	OK bool
	// Checked is the number of entries verified before stopping.
	Checked int64
	// HeadSeq is the sequence number recorded in the chain head.
	HeadSeq int64
	// FirstBroken describes the first inconsistency, when OK is false.
	FirstBroken *domain.ChainBreak
}

// VerifyChain recomputes every entry hash in sequence order and checks that
// each entry links to the previous one. It also checks that the chain head
// row points at the last entry, which detects deleted trailing entries.
//
// The scan reads committed data in batches without locking, so entries
// appended during the scan are simply not covered by this run.
func (s *Service) VerifyChain(ctx context.Context) (*ChainReport, error) {
	headSeq, headHash, err := s.store.ChainHead(ctx)
	if err != nil {
		return nil, err
	}
	rep := &ChainReport{HeadSeq: headSeq}
	prevSeq, prevHash := int64(0), domain.GenesisHash
scan:
	for prevSeq < headSeq {
		batch, err := s.store.ListEntriesBySeq(ctx, prevSeq, auditBatchSize)
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		for _, e := range batch {
			if e.Seq > headSeq {
				// Appended after we read the head; this run stops there.
				break scan
			}
			if b := domain.VerifyLink(prevSeq, prevHash, e); b != nil {
				rep.FirstBroken = b
				return rep, nil
			}
			rep.Checked++
			prevSeq, prevHash = e.Seq, e.Hash
		}
	}
	if prevSeq != headSeq || prevHash != headHash {
		rep.FirstBroken = &domain.ChainBreak{
			Seq:    prevSeq + 1,
			Reason: "chain head does not match the last entry (entries may have been deleted)",
		}
		return rep, nil
	}
	rep.OK = true
	return rep, nil
}
