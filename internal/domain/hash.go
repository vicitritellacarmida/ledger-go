package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// GenesisHash is the "previous hash" of the very first entry in the chain.
var GenesisHash = strings.Repeat("0", 64)

// canonicalEntry fixes the field order and formatting of the hashed content.
// encoding/json writes struct fields in declaration order, so the output is
// deterministic. Changing this struct changes every hash, so bump V when you
// do and teach the verifier about both versions.
type canonicalEntry struct {
	V              int                `json:"v"`
	Seq            int64              `json:"seq"`
	Type           string             `json:"type"`
	Description    string             `json:"description"`
	ExternalRef    string             `json:"external_ref"`
	EffectiveDate  string             `json:"effective_date"`
	ReversalOf     *int64             `json:"reversal_of"`
	RelatedEntryID *int64             `json:"related_entry_id"`
	HoldID         *int64             `json:"hold_id"`
	CreatedAt      string             `json:"created_at"`
	Postings       []canonicalPosting `json:"postings"`
}

type canonicalPosting struct {
	Line     int    `json:"line"`
	Account  string `json:"account"`
	Currency string `json:"currency"`
	Amount   int64  `json:"amount"`
	Role     string `json:"role"`
}

// CanonicalContent returns the bytes that are hashed for an entry. The entry
// id is not included because it is assigned by the database; Seq, which the
// ledger assigns before insert, takes its place.
func CanonicalContent(e *Entry) []byte {
	c := canonicalEntry{
		V:              1,
		Seq:            e.Seq,
		Type:           string(e.Type),
		Description:    e.Description,
		ExternalRef:    e.ExternalRef,
		EffectiveDate:  FormatDate(e.EffectiveDate),
		ReversalOf:     e.ReversalOf,
		RelatedEntryID: e.RelatedEntryID,
		HoldID:         e.HoldID,
		CreatedAt:      e.CreatedAt.UTC().Format(time.RFC3339Nano),
		Postings:       make([]canonicalPosting, len(e.Postings)),
	}
	for i, p := range e.Postings {
		c.Postings[i] = canonicalPosting{
			Line: p.LineNo, Account: p.AccountCode, Currency: p.Currency, Amount: p.Amount, Role: p.Role,
		}
	}
	b, err := json.Marshal(c)
	if err != nil {
		// Marshal of this plain struct cannot fail.
		panic(err)
	}
	return b
}

// ComputeHash returns hex(sha256(prevHash + "\n" + canonical content)).
// Because each hash covers the previous one, changing any stored entry
// breaks the hash of that entry and every entry after it.
func ComputeHash(prevHash string, e *Entry) string {
	h := sha256.New()
	h.Write([]byte(prevHash))
	h.Write([]byte("\n"))
	h.Write(CanonicalContent(e))
	return hex.EncodeToString(h.Sum(nil))
}

// SealEntry sets Seq, PrevHash and Hash on a new entry that follows the
// chain head (lastSeq, lastHash). CreatedAt must already be set.
func SealEntry(e *Entry, lastSeq int64, lastHash string) {
	e.Seq = lastSeq + 1
	e.PrevHash = lastHash
	e.Hash = ComputeHash(lastHash, e)
}

// ChainBreak describes the first inconsistency found by VerifyLink.
type ChainBreak struct {
	EntryID int64
	Seq     int64
	Reason  string
}

// VerifyLink checks one entry against the hash of the entry before it.
func VerifyLink(prevSeq int64, prevHash string, e *Entry) *ChainBreak {
	switch {
	case e.Seq != prevSeq+1:
		return &ChainBreak{EntryID: e.ID, Seq: e.Seq, Reason: "sequence gap: expected seq " + itoa(prevSeq+1)}
	case e.PrevHash != prevHash:
		return &ChainBreak{EntryID: e.ID, Seq: e.Seq, Reason: "prev_hash does not match the hash of the previous entry"}
	case ComputeHash(prevHash, e) != e.Hash:
		return &ChainBreak{EntryID: e.ID, Seq: e.Seq, Reason: "stored hash does not match entry content"}
	}
	return nil
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
