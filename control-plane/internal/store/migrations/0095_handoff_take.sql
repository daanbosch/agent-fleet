-- The recipient takes a handoff offer before launching (issue #2020, ADR 0057 decision 6).
--
-- Acceptance used to be reported after the launch, so an offer the sender withdrew while the
-- recipient's launch dialog was open was launched anyway and the same work ran twice. Taking
-- the offer is a conditional update made before the session is created: while take_token is
-- set and taken_at is recent, the sender's withdraw, the recipient's decline and expiry are
-- refused, and only the holder of the token can accept it or give it back. The status stays
-- 'pending' while taken, so the status CHECK and the one-pending index are untouched.
--
-- take_confirmed_at is set by the CP when the session create carrying the take reaches the
-- Agent: the commit point. A confirmed take holds until its holder accepts or releases it. An
-- unconfirmed take older than the take window (store.HandoffTakeWindow) no longer holds the
-- offer against the sender: it covers a Console that closed before launching, and if that
-- Console wakes up later its create fails the confirm instead of starting the work. A
-- confirmed take is never decided under: withdraw, decline and expiry wait for it, and an
-- unshare erases the body but keeps the take.
--
-- Forward compatible: the columns default to '', which every existing row reads as untaken.
ALTER TABLE session_handoff_offer ADD COLUMN take_token TEXT NOT NULL DEFAULT '';
ALTER TABLE session_handoff_offer ADD COLUMN taken_at TEXT NOT NULL DEFAULT '';
ALTER TABLE session_handoff_offer ADD COLUMN take_confirmed_at TEXT NOT NULL DEFAULT '';
