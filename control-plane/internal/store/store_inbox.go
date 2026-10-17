package store

// Store for the member request inbox (ADR 0111 decisions 2, 3, 8-10, P1-A).
//
// Every state change is one conditional UPDATE on the item row, and that row is the first
// thing each transaction writes. On Postgres the row lock is what serialises two members
// acting on the same item: a statement that waited for it re-evaluates its WHERE against the
// committed row, so "claim while open" has exactly one winner however many arrive at once.
// SQLite runs on one connection, so its transactions are serial anyway. Counting and then
// updating would let two concurrent claims both pass (docs/log/130 §130.3).
//
// Lock order, which every transaction here keeps so that none can deadlock another:
// membership rows in id order, then item rows in id order, then mailbox rows in
// membership-id order, then participant rows. Only a send and a removal lock membership rows:
// a send locks its sender's and recipients' before it checks they are active, so a removal of
// any of them either commits first (and the send sees it) or waits for the send (and then
// finds the new item). A send has no item to lock yet. Block and settings changes lock the
// owner's mailbox, which orders them against a send to that member. Measured on Postgres: a
// read that took the participant row before the mailbox deadlocked against a claim, and a
// removal that took a mailbox before a DM's item row deadlocked against a read (40P01 both).

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
)

// InboxItem is one inbox item. State is "" for a DM.
type InboxItem struct {
	ID, TenantID, SenderMembershipID, Kind, State, AssigneeMembershipID string
	Ciphertext, KeyRef                                                  string
	CreatedAt, UpdatedAt, UpdatedBy, FinalAt                            string
	// The session the assignee launched from the request, recorded by the CP once the
	// launch succeeded. SessionMembershipID is who launched it.
	SessionMembershipID, SessionName, SessionDir, SessionBranch, LaunchedAt string
	// The launch in flight, if any. LaunchState is "" (none), "reserved" (the assignee pressed
	// launch; the Agent has not been asked) or "creating" (the CP confirmed the claim and asked
	// the Agent, so a session may exist). LaunchToken is that create's Agent idempotency key,
	// LaunchBy who launched, and LaunchPendingUntil when a reservation lapses.
	LaunchPendingUntil, LaunchToken, LaunchBy, LaunchState string
	PRURL, PRBranch                                        string
}

// InboxParticipant is one person's side of an item: the sender's row, or one recipient's.
type InboxParticipant struct {
	ItemID, MembershipID, Role, Delivery string
	Seq                                  int64
}

// InboxEntry is an item as one participant sees it.
type InboxEntry struct {
	Item InboxItem
	Mine InboxParticipant
}

// InboxMailbox is one membership's counter, read cursor and receive controls.
type InboxMailbox struct {
	MembershipID     string
	LastSeq, ReadSeq int64
	AcceptFrom       string // "tenant" | "chosen"
	DND              bool
}

// InboxSendLimits are the per-sender limits checked inside the send transaction.
type InboxSendLimits struct {
	PerHour        int
	HourStart      string // RFC3339: items created at or after it count against PerHour
	PendingPerPair int
}

// InboxSendResult reports a send. Created=false comes with the reason.
type InboxSendResult struct {
	Created     bool
	RateLimited bool
	PendingFull []string // recipient membership ids whose pair limit is reached
	// Invalid: a recipient (or the sender) is not an active member of the item's tenant.
	Invalid bool
	// NotAccepting: recipient membership ids that block the sender or accept from chosen
	// members only, without the sender among them.
	NotAccepting []string
}

// InboxRetention is the cut-off for reads and the purge: a DM created before DMBefore and a
// request that reached its final state before FinalBefore are expired.
type InboxRetention struct {
	DMBefore, FinalBefore string
}

// InboxStore is the inbox's narrow view of the store.
type InboxStore interface {
	CreateInboxItem(ctx context.Context, it InboxItem, recipients []string, lim InboxSendLimits) (InboxSendResult, error)
	CreateInboxItemWith(ctx context.Context, it InboxItem, recipients []string, lim InboxSendLimits, extras InboxSendExtras) (InboxSendResult, error)
	ListInboxAttachments(ctx context.Context, itemID string) ([]InboxAttachment, error)
	TakeInInboxRecipe(ctx context.Context, itemID, membershipID string, m Memo) (Memo, InboxTakeIn, error)
	FindMemoByOrigin(ctx context.Context, itemID, membershipID string) (Memo, bool, error)
	RecordInboxSessionOrigin(ctx context.Context, membershipID, sessionName, itemID, senderKey string) error
	ListInboxSessionOrigins(ctx context.Context, membershipID string) (map[string]InboxSessionOrigin, error)
	GetInboxItem(ctx context.Context, id string) (InboxItem, bool, error)
	ListInboxParticipants(ctx context.Context, itemID string) ([]InboxParticipant, error)
	ListInboxEntries(ctx context.Context, membershipID string, ret InboxRetention) ([]InboxEntry, error)
	CountInboxUnseen(ctx context.Context, membershipID string, ret InboxRetention) (int, error)
	InboxEligibleRecipients(ctx context.Context, itemID string) (map[string]bool, error)

	ClaimInboxItem(ctx context.Context, id, membershipID, now string) (bool, error)
	DeclineInboxItem(ctx context.Context, id, membershipID, now string) (bool, error)
	ReleaseInboxItem(ctx context.Context, id, membershipID, now string) (bool, error)
	CompleteInboxItem(ctx context.Context, id, membershipID, now string) (bool, error)
	WithdrawInboxItem(ctx context.Context, id, membershipID, now string) (bool, error)
	MarkInboxRead(ctx context.Context, id, membershipID, now string) (bool, error)

	BeginInboxLaunch(ctx context.Context, id, membershipID, token, now, until string) (bool, error)
	ConfirmInboxLaunch(ctx context.Context, id, membershipID, token string) (bool, error)
	FinishInboxLaunch(ctx context.Context, id, membershipID, token, name, dir, branch, now string) (bool, error)
	AbortInboxLaunch(ctx context.Context, id, token string) error
	SetInboxPRLink(ctx context.Context, id, membershipID, url, branch, now string) (bool, error)

	GetInboxMailbox(ctx context.Context, membershipID string) (InboxMailbox, error)
	SetInboxReadCursor(ctx context.Context, membershipID string, seq int64) (InboxMailbox, error)
	PutInboxSettings(ctx context.Context, membershipID, acceptFrom string, dnd bool, allowed []string) error
	ListInboxSenderRules(ctx context.Context, membershipID string) (map[string]string, error)
	SetInboxBlock(ctx context.Context, membershipID, senderMembershipID string, blocked bool) error

	ReconcileInbox(ctx context.Context, tenantID, now string) ([]string, error)
	PurgeInbox(ctx context.Context, ret InboxRetention) (int, error)
}

const inboxItemCols = `id,tenant_id,sender_membership_id,kind,state,assignee_membership_id,ciphertext,key_ref,
	created_at,updated_at,updated_by,final_at,session_membership_id,session_name,session_dir,session_branch,
	launched_at,launch_pending_until,launch_token,launch_by,launch_state,pr_url,pr_branch`

func inboxItemColsAs(alias string) string {
	cols := strings.Split(strings.Join(strings.Fields(inboxItemCols), ""), ",")
	for i, c := range cols {
		cols[i] = alias + "." + c
	}
	return strings.Join(cols, ",")
}

func scanInboxItem(row interface{ Scan(...any) error }, extra ...any) (InboxItem, error) {
	var it InboxItem
	dst := []any{&it.ID, &it.TenantID, &it.SenderMembershipID, &it.Kind, &it.State, &it.AssigneeMembershipID,
		&it.Ciphertext, &it.KeyRef, &it.CreatedAt, &it.UpdatedAt, &it.UpdatedBy, &it.FinalAt,
		&it.SessionMembershipID, &it.SessionName, &it.SessionDir, &it.SessionBranch,
		&it.LaunchedAt, &it.LaunchPendingUntil, &it.LaunchToken, &it.LaunchBy, &it.LaunchState, &it.PRURL, &it.PRBranch}
	return it, row.Scan(append(dst, extra...)...)
}

// inboxEligible is the predicate "participant p can still take item i": a recipient who
// has not declined, whose membership is active, and who has not blocked the sender
// (decision 2). Removed and blocking recipients are not counted when deciding whether
// every recipient has declined.
const inboxEligible = `p.role='recipient' AND p.delivery<>'declined'
	AND EXISTS (SELECT 1 FROM membership m WHERE m.id=p.membership_id AND m.status='active')
	AND NOT EXISTS (SELECT 1 FROM inbox_sender_rule r
	                WHERE r.membership_id=p.membership_id AND r.sender_membership_id=i.sender_membership_id
	                  AND r.rule='block')`

// inboxClaimGuard is the condition that makes a claim conditional. It is a variable only so
// the race test's positive control can remove it and show that the test then fails.
//
// It has to test the target row itself, never a second scan of inbox_item in a subquery: a
// Postgres UPDATE that waited for the row lock re-checks its own row against the committed
// version, but a subquery keeps reading the statement's original snapshot, where the item
// is still open, and every waiting claim would then win (measured: with the guard moved into
// the subquery, TestInboxClaimRaceHasOneWinner fails on Postgres and passes on SQLite).
var inboxClaimGuard = `AND state='open'`

// inboxDropReservation clears a launch that has not asked the Agent yet. A withdraw or a
// done keeps a launch that is already creating, so the session it makes is still recorded
// on the card.
const inboxDropReservation = `launch_token=CASE WHEN launch_state='reserved' THEN '' ELSE launch_token END,
	launch_by=CASE WHEN launch_state='reserved' THEN '' ELSE launch_by END,
	launch_state=CASE WHEN launch_state='reserved' THEN '' ELSE launch_state END`

// inboxDropLaunch clears any launch record. A release hands the request back, so a launch
// the assignee had in flight, finished or not, is no longer this request's: that is also the
// way out of a launch whose outcome is unknown.
const inboxDropLaunch = `launch_token='',launch_by='',launch_state='',launch_pending_until=''`

// inboxNotExpired filters expired rows before anything is decrypted (decision 9).
const inboxNotExpired = `((i.kind='dm' AND i.created_at>=?) OR (i.kind='request' AND (i.final_at='' OR i.final_at>=?)))`

// lockMailboxes creates the mailbox rows of ids that do not have one yet and locks all of
// them, in id order.
func lockMailboxes(ctx context.Context, tx *sqlTx, ids []string) error {
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	for i, id := range sorted {
		if i > 0 && sorted[i-1] == id {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO inbox_mailbox(membership_id) VALUES(?) ON CONFLICT(membership_id) DO NOTHING`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE inbox_mailbox SET last_seq=last_seq WHERE membership_id=?`, id); err != nil {
			return err
		}
	}
	return nil
}

// lockMemberships locks membership rows in id order. Statements after it read the status
// those rows have once the lock is held, including a removal that committed while waiting.
func lockMemberships(ctx context.Context, tx *sqlTx, ids []string) error {
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	for i, id := range sorted {
		if i > 0 && sorted[i-1] == id {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE membership SET status=status WHERE id=?`, id); err != nil {
			return err
		}
	}
	return nil
}

// bumpInboxItem gives every participant of an item the next number in their own sequence,
// which is what moves their stream and their badge.
func bumpInboxItem(ctx context.Context, tx *sqlTx, itemID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT membership_id FROM inbox_participant WHERE item_id=?`, itemID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if err := lockMailboxes(ctx, tx, ids); err != nil {
		return err
	}
	sort.Strings(ids)
	for _, id := range ids {
		var seq int64
		if err := tx.QueryRowContext(ctx,
			`UPDATE inbox_mailbox SET last_seq=last_seq+1 WHERE membership_id=? RETURNING last_seq`, id).Scan(&seq); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE inbox_participant SET seq=? WHERE item_id=? AND membership_id=?`, seq, itemID, id); err != nil {
			return err
		}
	}
	return nil
}

// inboxTx runs f in a transaction and commits it when f reports changed=true. Nothing changed
// means nothing to commit, and the rollback releases what f locked.
func (s *SQL) inboxTx(ctx context.Context, f func(tx *sqlTx) (bool, error)) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	changed, err := f(tx)
	if err != nil || !changed {
		return false, err
	}
	return true, tx.Commit()
}

// CreateInboxItem inserts an item, its sender row and one row per recipient, and checks the
// sender's limits in the same transaction. Every mailbox involved is locked first, so two
// sends by the same sender queue behind each other and cannot both pass a count.
func (s *SQL) CreateInboxItem(ctx context.Context, it InboxItem, recipients []string, lim InboxSendLimits) (InboxSendResult, error) {
	return s.CreateInboxItemWith(ctx, it, recipients, lim, InboxSendExtras{})
}

// CreateInboxItemWith is CreateInboxItem with the item's attachments and the shares a
// session attachment grants, all in the one transaction: if any part fails, nothing of the
// item, its attachments or its shares is kept (decision 5).
func (s *SQL) CreateInboxItemWith(ctx context.Context, it InboxItem, recipients []string, lim InboxSendLimits, extras InboxSendExtras) (InboxSendResult, error) {
	var out InboxSendResult
	_, err := s.inboxTx(ctx, func(tx *sqlTx) (bool, error) {
		if err := lockMemberships(ctx, tx, append([]string{it.SenderMembershipID}, recipients...)); err != nil {
			return false, err
		}
		// Who may receive is decided again here, under the membership locks a removal and a
		// hard delete also take, and before any mailbox: a member removed or deleted while the
		// body was being sealed is refused without this send touching their mailbox, which a
		// hard delete may hold. The receive rules are read after the mailbox locks a block and
		// a settings change also take.
		if contains(recipients, it.SenderMembershipID) {
			out.Invalid = true
			return false, nil
		}
		for _, mid := range append([]string{it.SenderMembershipID}, recipients...) {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM membership WHERE id=? AND tenant_id=? AND status='active'`,
				mid, it.TenantID).Scan(&n); err != nil {
				return false, err
			}
			if n == 0 {
				out.Invalid = true
				return false, nil
			}
		}
		if err := lockMailboxes(ctx, tx, append([]string{it.SenderMembershipID}, recipients...)); err != nil {
			return false, err
		}
		for _, rid := range recipients {
			var acceptFrom, rule string
			if err := tx.QueryRowContext(ctx, `SELECT b.accept_from, COALESCE((SELECT r.rule FROM inbox_sender_rule r
				WHERE r.membership_id=b.membership_id AND r.sender_membership_id=?),'')
				FROM inbox_mailbox b WHERE b.membership_id=?`, it.SenderMembershipID, rid).Scan(&acceptFrom, &rule); err != nil {
				return false, err
			}
			if rule == "block" || (acceptFrom == "chosen" && rule != "allow") {
				out.NotAccepting = append(out.NotAccepting, rid)
			}
		}
		if len(out.NotAccepting) > 0 {
			return false, nil
		}
		var sent int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM inbox_item WHERE sender_membership_id=? AND created_at>=?`,
			it.SenderMembershipID, lim.HourStart).Scan(&sent); err != nil {
			return false, err
		}
		if sent >= lim.PerHour {
			out.RateLimited = true
			return false, nil
		}
		for _, rid := range recipients {
			var pending int
			// Pending for a pair: a request still open or claimed that this recipient has
			// not declined, or a DM they have not read. A release keeps a request pending
			// (decision 9).
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox_item i
				JOIN inbox_participant p ON p.item_id=i.id
				WHERE i.sender_membership_id=? AND p.membership_id=? AND p.role='recipient'
				  AND ((i.kind='request' AND i.state IN ('open','claimed') AND p.delivery<>'declined')
				    OR (i.kind='dm' AND p.delivery='unread'))`,
				it.SenderMembershipID, rid).Scan(&pending); err != nil {
				return false, err
			}
			if pending >= lim.PendingPerPair {
				out.PendingFull = append(out.PendingFull, rid)
			}
		}
		if len(out.PendingFull) > 0 {
			return false, nil
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO inbox_item
			(id,tenant_id,sender_membership_id,kind,state,ciphertext,key_ref,created_at,updated_at,updated_by)
			VALUES(?,?,?,?,?,?,?,?,?,?)`,
			it.ID, it.TenantID, it.SenderMembershipID, it.Kind, it.State, it.Ciphertext, it.KeyRef,
			it.CreatedAt, it.CreatedAt, it.SenderMembershipID); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO inbox_participant(item_id,membership_id,role,delivery)
			VALUES(?,?,'sender','read')`, it.ID, it.SenderMembershipID); err != nil {
			return false, err
		}
		for _, rid := range recipients {
			if _, err := tx.ExecContext(ctx, `INSERT INTO inbox_participant(item_id,membership_id,role,delivery)
				VALUES(?,?,'recipient','unread')`, it.ID, rid); err != nil {
				return false, err
			}
		}
		if err := insertInboxExtras(ctx, tx, it, extras); err != nil {
			return false, err
		}
		if err := bumpInboxItem(ctx, tx, it.ID); err != nil {
			return false, err
		}
		out.Created = true
		return true, nil
	})
	return out, err
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (s *SQL) GetInboxItem(ctx context.Context, id string) (InboxItem, bool, error) {
	it, err := scanInboxItem(s.db.QueryRowContext(ctx, `SELECT `+inboxItemCols+` FROM inbox_item WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return InboxItem{}, false, nil
	}
	return it, err == nil, err
}

func (s *SQL) ListInboxParticipants(ctx context.Context, itemID string) ([]InboxParticipant, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT item_id,membership_id,role,delivery,seq
		FROM inbox_participant WHERE item_id=? ORDER BY role DESC, membership_id`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []InboxParticipant{}
	for rows.Next() {
		var p InboxParticipant
		if err := rows.Scan(&p.ItemID, &p.MembershipID, &p.Role, &p.Delivery, &p.Seq); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListInboxEntries returns every unexpired item the membership takes part in, newest change
// first.
func (s *SQL) ListInboxEntries(ctx context.Context, membershipID string, ret InboxRetention) ([]InboxEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+inboxItemColsAs("i")+`,p.item_id,p.membership_id,p.role,p.delivery,p.seq
		FROM inbox_participant p JOIN inbox_item i ON i.id=p.item_id
		WHERE p.membership_id=? AND `+inboxNotExpired+`
		ORDER BY p.seq DESC`, membershipID, ret.DMBefore, ret.FinalBefore)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []InboxEntry{}
	for rows.Next() {
		var e InboxEntry
		it, err := scanInboxItem(rows, &e.Mine.ItemID, &e.Mine.MembershipID, &e.Mine.Role, &e.Mine.Delivery, &e.Mine.Seq)
		if err != nil {
			return nil, err
		}
		e.Item = it
		out = append(out, e)
	}
	return out, rows.Err()
}

// CountInboxUnseen counts the items that changed after the read cursor through somebody
// else's action. A member's own send or claim does not light their own badge.
func (s *SQL) CountInboxUnseen(ctx context.Context, membershipID string, ret InboxRetention) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox_participant p JOIN inbox_item i ON i.id=p.item_id
		WHERE p.membership_id=? AND i.updated_by<>? AND `+inboxNotExpired+`
		  AND p.seq > COALESCE((SELECT read_seq FROM inbox_mailbox b WHERE b.membership_id=p.membership_id),0)`,
		membershipID, membershipID, ret.DMBefore, ret.FinalBefore).Scan(&n)
	return n, err
}

// InboxEligibleRecipients returns the recipients who can still take the item.
func (s *SQL) InboxEligibleRecipients(ctx context.Context, itemID string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.membership_id FROM inbox_participant p
		JOIN inbox_item i ON i.id=p.item_id WHERE p.item_id=? AND `+inboxEligible, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// ClaimInboxItem makes the caller the assignee of an open request: one conditional update
// on the state still being open, with the caller still eligible (decision 2).
func (s *SQL) ClaimInboxItem(ctx context.Context, id, membershipID, now string) (bool, error) {
	return s.inboxTx(ctx, func(tx *sqlTx) (bool, error) {
		ok, err := affected(tx.ExecContext(ctx, `UPDATE inbox_item SET state='claimed',assignee_membership_id=?,
			updated_at=?,updated_by=?
			WHERE id=? AND kind='request' `+inboxClaimGuard+` AND EXISTS (SELECT 1 FROM inbox_participant p
			  JOIN inbox_item i ON i.id=p.item_id
			  WHERE p.item_id=inbox_item.id AND p.membership_id=? AND `+inboxEligible+`)`,
			membershipID, now, membershipID, id, membershipID))
		if err != nil || !ok {
			return false, err
		}
		return true, bumpInboxItem(ctx, tx, id)
	})
}

// finishIfNoneEligible turns an open request whose recipients can no longer take it into
// declined (docs/log/130 §130.8 item 2). The item row is already locked by the caller.
func finishIfNoneEligible(ctx context.Context, tx *sqlTx, id, now string) error {
	_, err := tx.ExecContext(ctx, `UPDATE inbox_item SET state='declined',final_at=?,updated_at=?,updated_by=''
		WHERE id=? AND state='open' AND NOT EXISTS (SELECT 1 FROM inbox_participant p
		  JOIN inbox_item i ON i.id=p.item_id WHERE p.item_id=inbox_item.id AND `+inboxEligible+`)`,
		now, now, id)
	return err
}

// DeclineInboxItem records one recipient's decline of an open request. The item row is
// written first: without that lock two recipients declining at once on Postgres would each
// miss the other's decline, and the request would stay open with nobody left to take it.
func (s *SQL) DeclineInboxItem(ctx context.Context, id, membershipID, now string) (bool, error) {
	return s.inboxTx(ctx, func(tx *sqlTx) (bool, error) {
		ok, err := affected(tx.ExecContext(ctx, `UPDATE inbox_item SET updated_at=?,updated_by=?
			WHERE id=? AND kind='request' AND state='open'`, now, membershipID, id))
		if err != nil || !ok {
			return false, err
		}
		ok, err = affected(tx.ExecContext(ctx, `UPDATE inbox_participant SET delivery='declined'
			WHERE item_id=? AND membership_id=? AND role='recipient' AND delivery<>'declined'`, id, membershipID))
		if err != nil || !ok {
			return false, err
		}
		if err := finishIfNoneEligible(ctx, tx, id, now); err != nil {
			return false, err
		}
		return true, bumpInboxItem(ctx, tx, id)
	})
}

// ReleaseInboxItem hands a claimed request back: the assignee only, back to open.
func (s *SQL) ReleaseInboxItem(ctx context.Context, id, membershipID, now string) (bool, error) {
	return s.inboxTx(ctx, func(tx *sqlTx) (bool, error) {
		ok, err := affected(tx.ExecContext(ctx, `UPDATE inbox_item SET state='open',assignee_membership_id='',
			`+inboxDropLaunch+`,updated_at=?,updated_by=?
			WHERE id=? AND state='claimed' AND assignee_membership_id=?`, now, membershipID, id, membershipID))
		if err != nil || !ok {
			return false, err
		}
		// The assignee who released may have been the last one able to take it.
		if err := finishIfNoneEligible(ctx, tx, id, now); err != nil {
			return false, err
		}
		return true, bumpInboxItem(ctx, tx, id)
	})
}

// CompleteInboxItem marks a claimed request done: the assignee only. The assignee is kept.
func (s *SQL) CompleteInboxItem(ctx context.Context, id, membershipID, now string) (bool, error) {
	return s.inboxTx(ctx, func(tx *sqlTx) (bool, error) {
		ok, err := affected(tx.ExecContext(ctx, `UPDATE inbox_item SET state='done',final_at=?,`+inboxDropReservation+`,
			updated_at=?,updated_by=?
			WHERE id=? AND state='claimed' AND assignee_membership_id=?`, now, now, membershipID, id, membershipID))
		if err != nil || !ok {
			return false, err
		}
		return true, bumpInboxItem(ctx, tx, id)
	})
}

// WithdrawInboxItem is the sender's withdrawal of an open or claimed request. The assignee
// stays on record so they can see that what they took was withdrawn.
func (s *SQL) WithdrawInboxItem(ctx context.Context, id, membershipID, now string) (bool, error) {
	return s.inboxTx(ctx, func(tx *sqlTx) (bool, error) {
		ok, err := affected(tx.ExecContext(ctx, `UPDATE inbox_item SET state='withdrawn',final_at=?,`+inboxDropReservation+`,
			updated_at=?,updated_by=?
			WHERE id=? AND state IN ('open','claimed') AND sender_membership_id=?`, now, now, membershipID, id, membershipID))
		if err != nil || !ok {
			return false, err
		}
		return true, bumpInboxItem(ctx, tx, id)
	})
}

// MarkInboxRead moves the caller's delivery from unread to read. Only the caller's counter
// moves, so their other tabs refresh. The row's own sequence number stays: giving it a new
// one would count the item as unseen again, the opposite of having read it.
func (s *SQL) MarkInboxRead(ctx context.Context, id, membershipID, now string) (bool, error) {
	return s.inboxTx(ctx, func(tx *sqlTx) (bool, error) {
		// Item, then mailbox, then participant: the order of every other transaction here.
		if _, err := tx.ExecContext(ctx, `UPDATE inbox_item SET updated_at=updated_at WHERE id=?`, id); err != nil {
			return false, err
		}
		if err := lockMailboxes(ctx, tx, []string{membershipID}); err != nil {
			return false, err
		}
		ok, err := affected(tx.ExecContext(ctx, `UPDATE inbox_participant SET delivery='read'
			WHERE item_id=? AND membership_id=? AND delivery='unread'`, id, membershipID))
		if err != nil || !ok {
			return false, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE inbox_mailbox SET last_seq=last_seq+1 WHERE membership_id=?`, membershipID)
		return err == nil, err
	})
}

// BeginInboxLaunch reserves a launch for the current assignee under token. It refuses while
// another launch is reserved or creating, and once this assignee has a session recorded, so
// a double click or a retry creates one session (docs/log/130 R-8). A reservation that
// outlived until (a CP that stopped between reserving and confirming) can be taken over.
func (s *SQL) BeginInboxLaunch(ctx context.Context, id, membershipID, token, now, until string) (bool, error) {
	return affected(s.db.ExecContext(ctx, `UPDATE inbox_item SET launch_token=?,launch_by=?,launch_state='reserved',
		launch_pending_until=?
		WHERE id=? AND kind='request' AND state='claimed' AND assignee_membership_id=?
		  AND NOT (session_membership_id=? AND session_name<>'')
		  AND (launch_state='' OR (launch_state='reserved' AND launch_pending_until<?))`,
		token, membershipID, until, id, membershipID, membershipID, now))
}

// ConfirmInboxLaunch is the commit point of a launch, run immediately before the Agent is
// asked: the reservation is still this one, the request still claimed, the caller still its
// assignee. A release or a withdraw that lands before it wins and the Agent is never asked;
// one that lands after it orders after the launch, which decision 2 lets run.
func (s *SQL) ConfirmInboxLaunch(ctx context.Context, id, membershipID, token string) (bool, error) {
	return affected(s.db.ExecContext(ctx, `UPDATE inbox_item SET launch_state='creating'
		WHERE id=? AND launch_token=? AND launch_state='reserved' AND state='claimed' AND assignee_membership_id=?`,
		id, token, membershipID))
}

// FinishInboxLaunch records the session the confirmed launch created. It is recorded even
// when the request was withdrawn or done in the meantime: the session exists, and the card
// should say so. recorded=false means the launch is no longer this one (a release dropped it).
//
// The session's provenance badge (inbox_session_origin) is written in the same transaction,
// so the card and the badge are recorded together or not at all, whether the session's name
// came from the create's answer or from the idempotency lookup that recovers a lost one.
func (s *SQL) FinishInboxLaunch(ctx context.Context, id, membershipID, token, name, dir, branch, now string) (bool, error) {
	return s.inboxTx(ctx, func(tx *sqlTx) (bool, error) {
		ok, err := affected(tx.ExecContext(ctx, `UPDATE inbox_item SET session_membership_id=?,session_name=?,
			session_dir=?,session_branch=?,launched_at=?,`+inboxDropLaunch+`,updated_at=?,updated_by=?
			WHERE id=? AND launch_token=? AND launch_by=? AND launch_state='creating'`,
			membershipID, name, dir, branch, now, now, membershipID, id, token, membershipID))
		if err != nil || !ok {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO inbox_session_origin(membership_id,session_name,item_id,sender_key,created_at)
			SELECT ?, ?, i.id, COALESCE((SELECT d.user_key FROM membership m JOIN identity d ON d.id=m.identity_id
			                             WHERE m.id=i.sender_membership_id),''), ?
			FROM inbox_item i WHERE i.id=?
			ON CONFLICT(membership_id,session_name) DO UPDATE SET item_id=excluded.item_id,
			  sender_key=excluded.sender_key,created_at=excluded.created_at`, membershipID, name, now, id); err != nil {
			return false, err
		}
		return true, bumpInboxItem(ctx, tx, id)
	})
}

// AbortInboxLaunch clears a launch that is known to have created nothing: refused before the
// Agent was asked, or refused by the Agent itself. A launch whose outcome is unknown is never
// aborted, because a later launch under a new key could create a second session.
func (s *SQL) AbortInboxLaunch(ctx context.Context, id, token string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE inbox_item SET `+inboxDropLaunch+`
		WHERE id=? AND launch_token=?`, id, token)
	return err
}

// SetInboxPRLink records the PR and final branch by hand: the assignee only, while the
// request is claimed or done.
func (s *SQL) SetInboxPRLink(ctx context.Context, id, membershipID, url, branch, now string) (bool, error) {
	return s.inboxTx(ctx, func(tx *sqlTx) (bool, error) {
		ok, err := affected(tx.ExecContext(ctx, `UPDATE inbox_item SET pr_url=?,pr_branch=?,updated_at=?,updated_by=?
			WHERE id=? AND kind='request' AND state IN ('claimed','done') AND assignee_membership_id=?`,
			url, branch, now, membershipID, id, membershipID))
		if err != nil || !ok {
			return false, err
		}
		return true, bumpInboxItem(ctx, tx, id)
	})
}

func (s *SQL) GetInboxMailbox(ctx context.Context, membershipID string) (InboxMailbox, error) {
	mb := InboxMailbox{MembershipID: membershipID, AcceptFrom: "tenant"}
	var dnd int
	err := s.db.QueryRowContext(ctx, `SELECT last_seq,read_seq,accept_from,dnd FROM inbox_mailbox WHERE membership_id=?`,
		membershipID).Scan(&mb.LastSeq, &mb.ReadSeq, &mb.AcceptFrom, &dnd)
	if errors.Is(err, sql.ErrNoRows) {
		return mb, nil
	}
	mb.DND = dnd != 0
	return mb, err
}

// SetInboxReadCursor moves the read cursor forward to seq. It never moves backwards and
// never past the counter, so a stale tab cannot un-read and a forged number cannot hide
// what arrives next.
func (s *SQL) SetInboxReadCursor(ctx context.Context, membershipID string, seq int64) (InboxMailbox, error) {
	_, err := s.inboxTx(ctx, func(tx *sqlTx) (bool, error) {
		if err := lockMailboxes(ctx, tx, []string{membershipID}); err != nil {
			return false, err
		}
		_, err := tx.ExecContext(ctx, `UPDATE inbox_mailbox SET read_seq=CASE
			WHEN ? > last_seq THEN last_seq WHEN ? > read_seq THEN ? ELSE read_seq END
			WHERE membership_id=?`, seq, seq, seq, membershipID)
		return err == nil, err
	})
	if err != nil {
		return InboxMailbox{}, err
	}
	return s.GetInboxMailbox(ctx, membershipID)
}

// PutInboxSettings stores the receive controls. allowed replaces the chosen-member list and
// never touches a block.
func (s *SQL) PutInboxSettings(ctx context.Context, membershipID, acceptFrom string, dnd bool, allowed []string) error {
	_, err := s.inboxTx(ctx, func(tx *sqlTx) (bool, error) {
		if err := lockMailboxes(ctx, tx, []string{membershipID}); err != nil {
			return false, err
		}
		d := 0
		if dnd {
			d = 1
		}
		// The counter moves too: the mailbox the Console holds carries dnd and acceptFrom, and
		// it keeps the newer of two mailboxes by this counter, so a list read before the change
		// can never put the old setting back over the stream's new one.
		if _, err := tx.ExecContext(ctx, `UPDATE inbox_mailbox SET accept_from=?,dnd=?,last_seq=last_seq+1 WHERE membership_id=?`,
			acceptFrom, d, membershipID); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM inbox_sender_rule WHERE membership_id=? AND rule='allow'`,
			membershipID); err != nil {
			return false, err
		}
		for _, sid := range allowed {
			if _, err := tx.ExecContext(ctx, `INSERT INTO inbox_sender_rule(membership_id,sender_membership_id,rule)
				VALUES(?,?,'allow') ON CONFLICT(membership_id,sender_membership_id) DO NOTHING`, membershipID, sid); err != nil {
				return false, err
			}
		}
		return true, nil
	})
	return err
}

// ListInboxSenderRules maps sender membership id to "allow" or "block".
func (s *SQL) ListInboxSenderRules(ctx context.Context, membershipID string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sender_membership_id,rule FROM inbox_sender_rule WHERE membership_id=?`, membershipID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, rule string
		if err := rows.Scan(&id, &rule); err != nil {
			return nil, err
		}
		out[id] = rule
	}
	return out, rows.Err()
}

// SetInboxBlock blocks or unblocks a sender. Unblocking drops the rule, so a sender who was
// on the chosen list before the block has to be chosen again. The blocker's mailbox is locked
// first, so a send to them either commits before the block or sees it.
func (s *SQL) SetInboxBlock(ctx context.Context, membershipID, senderMembershipID string, blocked bool) error {
	_, err := s.inboxTx(ctx, func(tx *sqlTx) (bool, error) {
		if err := lockMailboxes(ctx, tx, []string{membershipID}); err != nil {
			return false, err
		}
		q := `DELETE FROM inbox_sender_rule WHERE membership_id=? AND sender_membership_id=? AND rule='block'`
		if blocked {
			q = `INSERT INTO inbox_sender_rule(membership_id,sender_membership_id,rule)
				VALUES(?,?,'block') ON CONFLICT(membership_id,sender_membership_id) DO UPDATE SET rule='block'`
		}
		_, err := tx.ExecContext(ctx, q, membershipID, senderMembershipID)
		return err == nil, err
	})
	return err
}

// ReconcileInbox applies the system rows of decision 2's state table to one tenant and
// returns the ids of the items it changed:
//
//   - the sender's membership is no longer active: open or claimed becomes withdrawn;
//   - the assignee's membership is no longer active, or the assignee blocked the sender:
//     claimed goes back to open;
//   - an open request nobody can take any more becomes declined.
//
// Each item is its own transaction, opened by the conditional update on its row, so this is
// safe to run from every replica's sweep and from a read at the same time.
func (s *SQL) ReconcileInbox(ctx context.Context, tenantID, now string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT i.id FROM inbox_item i
		WHERE i.tenant_id=? AND i.kind='request' AND i.state IN ('open','claimed') AND (
		  NOT EXISTS (SELECT 1 FROM membership m WHERE m.id=i.sender_membership_id AND m.status='active')
		  OR (i.state='claimed' AND (
		        NOT EXISTS (SELECT 1 FROM membership m WHERE m.id=i.assignee_membership_id AND m.status='active')
		        OR EXISTS (SELECT 1 FROM inbox_sender_rule r WHERE r.membership_id=i.assignee_membership_id
		                   AND r.sender_membership_id=i.sender_membership_id AND r.rule='block')))
		  OR (i.state='open' AND NOT EXISTS (SELECT 1 FROM inbox_participant p WHERE p.item_id=i.id AND `+inboxEligible+`)))`,
		tenantID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var changed []string
	for _, id := range ids {
		ok, err := s.inboxTx(ctx, func(tx *sqlTx) (bool, error) { return reconcileInboxItemTx(ctx, tx, id, now) })
		if err != nil {
			return changed, err
		}
		if ok {
			changed = append(changed, id)
		}
	}
	return changed, nil
}

// reconcileInboxItemTx applies the system rows to one request inside tx and reports whether
// it changed. Each statement is conditional on the item row, so it is safe to run again.
func reconcileInboxItemTx(ctx context.Context, tx *sqlTx, id, now string) (bool, error) {
	withdrawn, err := affected(tx.ExecContext(ctx, `UPDATE inbox_item SET state='withdrawn',final_at=?,
		`+inboxDropReservation+`,updated_at=?,updated_by=''
		WHERE id=? AND state IN ('open','claimed') AND NOT EXISTS (SELECT 1 FROM membership m
		  WHERE m.id=inbox_item.sender_membership_id AND m.status='active')`, now, now, id))
	if err != nil {
		return false, err
	}
	if withdrawn {
		return true, bumpInboxItem(ctx, tx, id)
	}
	released, err := affected(tx.ExecContext(ctx, `UPDATE inbox_item SET state='open',assignee_membership_id='',
		`+inboxDropLaunch+`,updated_at=?,updated_by=''
		WHERE id=? AND state='claimed' AND (
		  NOT EXISTS (SELECT 1 FROM membership m WHERE m.id=inbox_item.assignee_membership_id AND m.status='active')
		  OR EXISTS (SELECT 1 FROM inbox_sender_rule r WHERE r.membership_id=inbox_item.assignee_membership_id
		             AND r.sender_membership_id=inbox_item.sender_membership_id AND r.rule='block'))`, now, id))
	if err != nil {
		return false, err
	}
	// Lock the row even when nothing was released, so the check below sees every decline
	// committed before it.
	open, err := affected(tx.ExecContext(ctx, `UPDATE inbox_item SET updated_at=updated_at
		WHERE id=? AND state='open'`, id))
	if err != nil {
		return false, err
	}
	declined := false
	if open {
		declined, err = affected(tx.ExecContext(ctx, `UPDATE inbox_item SET state='declined',final_at=?,
			updated_at=?,updated_by=''
			WHERE id=? AND state='open' AND NOT EXISTS (SELECT 1 FROM inbox_participant p
			  JOIN inbox_item i ON i.id=p.item_id WHERE p.item_id=inbox_item.id AND `+inboxEligible+`)`,
			now, now, id))
		if err != nil {
			return false, err
		}
	}
	if !released && !declined {
		return false, nil
	}
	return true, bumpInboxItem(ctx, tx, id)
}

func queryIDs(ctx context.Context, tx *sqlTx, q string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// inboxMembershipInactiveTx is what a membership leaving does to the inbox, inside the same
// transaction that makes it inactive (decision 9 and the state table's system rows): the
// requests it sent are withdrawn, its claims go back to open, a request nobody can take any
// more is declined, and an item no active member takes part in any more is deleted, body and
// all. Done at the removal itself rather than by a later sweep, so a re-invite before that
// sweep cannot undo it: the sweep would read the membership as active and see nothing to do.
func inboxMembershipInactiveTx(ctx context.Context, tx *sqlTx, membershipID, now string) error {
	// The caller has already written this membership's row, so a send to or from it is
	// either committed (and its item is found here) or waiting behind this transaction.
	// Every item the member takes part in is locked before any mailbox, DMs and finished
	// requests too: any of them can reach the end of its life below, and a read holds the
	// item while it waits for a mailbox.
	all, err := queryIDs(ctx, tx, `SELECT item_id FROM inbox_participant WHERE membership_id=? ORDER BY item_id`, membershipID)
	if err != nil {
		return err
	}
	for _, id := range all {
		if _, err := tx.ExecContext(ctx, `UPDATE inbox_item SET updated_at=updated_at WHERE id=?`, id); err != nil {
			return err
		}
	}
	reqs, err := queryIDs(ctx, tx, `SELECT DISTINCT p.item_id FROM inbox_participant p JOIN inbox_item i ON i.id=p.item_id
		WHERE p.membership_id=? AND i.kind='request' AND i.state IN ('open','claimed') ORDER BY p.item_id`, membershipID)
	if err != nil {
		return err
	}
	people := []string{membershipID}
	for _, id := range reqs {
		ps, err := queryIDs(ctx, tx, `SELECT membership_id FROM inbox_participant WHERE item_id=?`, id)
		if err != nil {
			return err
		}
		people = append(people, ps...)
	}
	if err := lockMailboxes(ctx, tx, people); err != nil {
		return err
	}
	for _, id := range reqs {
		if _, err := reconcileInboxItemTx(ctx, tx, id, now); err != nil {
			return err
		}
	}
	ended, err := queryIDs(ctx, tx, `SELECT p.item_id FROM inbox_participant p WHERE p.membership_id=?
		AND NOT EXISTS (SELECT 1 FROM inbox_participant q JOIN membership m ON m.id=q.membership_id
		                WHERE q.item_id=p.item_id AND m.status='active')`, membershipID)
	if err != nil {
		return err
	}
	for _, id := range ended {
		if _, err := tx.ExecContext(ctx, `DELETE FROM inbox_participant WHERE item_id=?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM inbox_attachment WHERE item_id=?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM inbox_item WHERE id=?`, id); err != nil {
			return err
		}
	}
	return nil
}

// PurgeInbox deletes what has reached the end of its life (decision 9), whether or not
// anybody reads it and whether or not any workspace or browser is running:
//
//   - expired items (a DM past its retention from creation, a request past its retention
//     from its final state);
//   - items no active member takes part in any more: the body's life ends with the last
//     participant's membership, without waiting for the hard delete;
//   - participant, mailbox and rule rows whose membership no longer exists, which an older
//     CP's hard delete leaves behind because it does not know these tables.
//
// Running it twice deletes nothing the second time. It returns how many items it deleted.
func (s *SQL) PurgeInbox(ctx context.Context, ret InboxRetention) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM inbox_participant WHERE NOT EXISTS (SELECT 1 FROM membership m WHERE m.id=inbox_participant.membership_id)`,
		`DELETE FROM inbox_mailbox WHERE NOT EXISTS (SELECT 1 FROM membership m WHERE m.id=inbox_mailbox.membership_id)`,
		`DELETE FROM inbox_sender_rule WHERE NOT EXISTS (SELECT 1 FROM membership m WHERE m.id=inbox_sender_rule.membership_id)
		   OR NOT EXISTS (SELECT 1 FROM membership m WHERE m.id=inbox_sender_rule.sender_membership_id)`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return 0, err
		}
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM inbox_item WHERE
		(kind='dm' AND created_at<?)
		OR (kind='request' AND final_at<>'' AND final_at<?)
		OR NOT EXISTS (SELECT 1 FROM inbox_participant p JOIN membership m ON m.id=p.membership_id
		               WHERE p.item_id=inbox_item.id AND m.status='active')`, ret.DMBefore, ret.FinalBefore)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	// The participant rows and attachments of the items just deleted go with them. ON
	// DELETE CASCADE does that only where foreign keys are enforced, so it is spelled out.
	for _, q := range []string{
		`DELETE FROM inbox_participant WHERE NOT EXISTS (SELECT 1 FROM inbox_item i WHERE i.id=inbox_participant.item_id)`,
		`DELETE FROM inbox_attachment WHERE NOT EXISTS (SELECT 1 FROM inbox_item i WHERE i.id=inbox_attachment.item_id)`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return 0, err
		}
	}
	return int(n), tx.Commit()
}
