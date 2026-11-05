package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/vicitritellacarmida/ledger-go/internal/domain"
)

// LockChainHead locks the single chain_head row and returns the sequence
// number and hash of the last entry. Holding this lock serializes every
// transaction that appends to the journal.
func (s *Store) LockChainHead(ctx context.Context) (lastSeq int64, lastHash string, err error) {
	if !InTransaction(ctx) {
		return 0, "", errors.New("store: LockChainHead requires a transaction")
	}
	err = s.conn(ctx).QueryRowContext(ctx,
		"SELECT last_seq, last_hash FROM chain_head WHERE id = 1 FOR UPDATE").Scan(&lastSeq, &lastHash)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", errors.New("store: chain_head row is missing; run migrations")
	}
	return lastSeq, lastHash, err
}

// ChainHead reads the chain head without locking.
func (s *Store) ChainHead(ctx context.Context) (lastSeq int64, lastHash string, err error) {
	err = s.conn(ctx).QueryRowContext(ctx,
		"SELECT last_seq, last_hash FROM chain_head WHERE id = 1").Scan(&lastSeq, &lastHash)
	return lastSeq, lastHash, err
}

// UpdateChainHead moves the chain head to a newly inserted entry.
func (s *Store) UpdateChainHead(ctx context.Context, seq int64, hash string, now time.Time) error {
	_, err := s.conn(ctx).ExecContext(ctx,
		"UPDATE chain_head SET last_seq = ?, last_hash = ?, updated_at = ? WHERE id = 1", seq, hash, now)
	return err
}

// InsertEntry stores an entry and its postings and sets e.ID.
func (s *Store) InsertEntry(ctx context.Context, e *domain.Entry) error {
	q := s.conn(ctx)
	res, err := q.ExecContext(ctx, `INSERT INTO journal_entries
		(seq, type, description, external_ref, effective_date, reversal_of, related_entry_id, hold_id, prev_hash, hash, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Seq, string(e.Type), e.Description, nullString(e.ExternalRef), e.EffectiveDate,
		nullInt(e.ReversalOf), nullInt(e.RelatedEntryID), nullInt(e.HoldID), e.PrevHash, e.Hash, e.CreatedAt)
	if err != nil {
		if isDuplicate(err) && e.ReversalOf != nil {
			return domain.NewError(domain.KindConflict, domain.CodeAlreadyReversed, "entry %d has already been reversed", *e.ReversalOf)
		}
		return err
	}
	if e.ID, err = res.LastInsertId(); err != nil {
		return err
	}
	var sb strings.Builder
	sb.WriteString("INSERT INTO postings (entry_id, line_no, account_id, currency, amount, role, effective_date) VALUES ")
	args := make([]any, 0, len(e.Postings)*7)
	for i, p := range e.Postings {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString("(?, ?, ?, ?, ?, ?, ?)")
		args = append(args, e.ID, p.LineNo, p.AccountID, p.Currency, p.Amount, p.Role, e.EffectiveDate)
	}
	_, err = q.ExecContext(ctx, sb.String(), args...)
	return err
}

const entryColumns = `id, seq, type, description, external_ref, effective_date, reversal_of,
	related_entry_id, hold_id, prev_hash, hash, created_at`

func scanEntry(r rowScanner) (*domain.Entry, error) {
	var e domain.Entry
	var typ string
	var ext sql.NullString
	var rev, rel, hold sql.NullInt64
	if err := r.Scan(&e.ID, &e.Seq, &typ, &e.Description, &ext, &e.EffectiveDate, &rev, &rel, &hold,
		&e.PrevHash, &e.Hash, &e.CreatedAt); err != nil {
		return nil, err
	}
	e.Type = domain.EntryType(typ)
	e.ExternalRef = ext.String
	e.ReversalOf, e.RelatedEntryID, e.HoldID = ptrInt(rev), ptrInt(rel), ptrInt(hold)
	return &e, nil
}

// GetEntry loads an entry with its postings.
func (s *Store) GetEntry(ctx context.Context, id int64) (*domain.Entry, error) {
	e, err := scanEntry(s.conn(ctx).QueryRowContext(ctx,
		"SELECT "+entryColumns+" FROM journal_entries WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound(domain.CodeEntryNotFound, "entry %d not found", id)
	}
	if err != nil {
		return nil, err
	}
	if err := s.attachPostings(ctx, []*domain.Entry{e}); err != nil {
		return nil, err
	}
	return e, nil
}

// attachPostings loads the postings of several entries in one query.
func (s *Store) attachPostings(ctx context.Context, entries []*domain.Entry) error {
	if len(entries) == 0 {
		return nil
	}
	byID := make(map[int64]*domain.Entry, len(entries))
	args := make([]any, len(entries))
	for i, e := range entries {
		byID[e.ID] = e
		e.Postings = nil
		args[i] = e.ID
	}
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT p.entry_id, p.line_no, p.account_id, a.code, p.currency, p.amount, p.role
		FROM postings p JOIN accounts a ON a.id = p.account_id
		WHERE p.entry_id IN (`+placeholders(len(entries))+`) ORDER BY p.entry_id, p.line_no`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var entryID int64
		var p domain.Posting
		if err := rows.Scan(&entryID, &p.LineNo, &p.AccountID, &p.AccountCode, &p.Currency, &p.Amount, &p.Role); err != nil {
			return err
		}
		e := byID[entryID]
		e.Postings = append(e.Postings, p)
	}
	return rows.Err()
}

// FindReversal returns the id of the entry that reverses id, if any.
func (s *Store) FindReversal(ctx context.Context, id int64) (int64, bool, error) {
	var revID int64
	err := s.conn(ctx).QueryRowContext(ctx,
		"SELECT id FROM journal_entries WHERE reversal_of = ?", id).Scan(&revID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return revID, err == nil, err
}

// EntryFilter narrows ListEntries. Zero values mean "any".
type EntryFilter struct {
	Type        domain.EntryType
	ExternalRef string
	// BeforeID returns entries with a smaller id (newest first paging).
	BeforeID int64
	Limit    int
}

// ListEntries returns entries newest first with their postings.
func (s *Store) ListEntries(ctx context.Context, f EntryFilter) ([]*domain.Entry, error) {
	where := []string{"1 = 1"}
	var args []any
	if f.Type != "" {
		where = append(where, "type = ?")
		args = append(args, string(f.Type))
	}
	if f.ExternalRef != "" {
		where = append(where, "external_ref = ?")
		args = append(args, f.ExternalRef)
	}
	if f.BeforeID > 0 {
		where = append(where, "id < ?")
		args = append(args, f.BeforeID)
	}
	args = append(args, f.Limit)
	return s.queryEntries(ctx, "SELECT "+entryColumns+" FROM journal_entries WHERE "+
		strings.Join(where, " AND ")+" ORDER BY id DESC LIMIT ?", args...)
}

// ListEntriesBySeq returns up to limit entries with seq > afterSeq in chain
// order, with postings. It is used by the audit verifier.
func (s *Store) ListEntriesBySeq(ctx context.Context, afterSeq int64, limit int) ([]*domain.Entry, error) {
	return s.queryEntries(ctx, "SELECT "+entryColumns+" FROM journal_entries WHERE seq > ? ORDER BY seq LIMIT ?",
		afterSeq, limit)
}

func (s *Store) queryEntries(ctx context.Context, query string, args ...any) ([]*domain.Entry, error) {
	rows, err := s.conn(ctx).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var out []*domain.Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, s.attachPostings(ctx, out)
}

// SumRefunded returns the total already refunded for a payment entry:
// the credited amount of every refund linked to it that has not itself been
// reversed.
func (s *Store) SumRefunded(ctx context.Context, paymentID int64) (int64, error) {
	var total sql.NullInt64
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT -SUM(p.amount)
		FROM journal_entries e JOIN postings p ON p.entry_id = e.id
		WHERE e.related_entry_id = ? AND e.type = ? AND p.role = ?
		  AND NOT EXISTS (SELECT 1 FROM journal_entries r WHERE r.reversal_of = e.id)`,
		paymentID, string(domain.EntryRefund), domain.RolePayer).Scan(&total)
	return total.Int64, err
}
