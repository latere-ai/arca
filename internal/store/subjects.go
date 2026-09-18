// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"fmt"
	"time"
)

// Subject is one space Arca has seen, as something a person recognises. It
// is presentation only: no authorization reads it, and a subject that is not
// here is a subject like any other.
type Subject struct {
	// Subject is <issuer>|<sub>.
	Subject string
	// Display is the email or the name a verified claim carried, or empty.
	Display string
	// LastSeen is when a request last arrived as this subject.
	LastSeen time.Time
}

// Subjects is the query set over the subject directory.
type Subjects interface {
	// Touch records that the subject was seen, keeping the display it
	// already had when the caller has none.
	Touch(ctx context.Context, q Querier, subject, display string) error
	// Get reads one subject. A subject that has not been seen is
	// pgx.ErrNoRows.
	Get(ctx context.Context, q Querier, subject string) (Subject, error)
}

// subjects is the query set over Postgres.
type subjects struct{}

// NewSubjects answers the query set over Postgres.
func NewSubjects() Subjects { return subjects{} }

// Touch records the subject.
func (subjects) Touch(ctx context.Context, q Querier, subject, display string) error {
	_, err := q.Exec(ctx, `
		INSERT INTO subjects (subject, display) VALUES ($1, $2)
		ON CONFLICT (subject) DO UPDATE
		   SET display = CASE WHEN EXCLUDED.display <> '' THEN EXCLUDED.display ELSE subjects.display END,
		       last_seen = now()`, subject, display)
	if err != nil {
		return classify("record the subject", err)
	}
	return nil
}

// Get reads one subject.
func (subjects) Get(ctx context.Context, q Querier, subject string) (Subject, error) {
	var s Subject
	if err := q.QueryRow(ctx,
		`SELECT subject, display, last_seen FROM subjects WHERE subject = $1`, subject).
		Scan(&s.Subject, &s.Display, &s.LastSeen); err != nil {
		return Subject{}, fmt.Errorf("store: read the subject: %w", err)
	}
	return s, nil
}
