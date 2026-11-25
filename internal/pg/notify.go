package pg

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Announcements are hints between the gateways of an org, not a queue: a
// listener hears nothing announced while it was not listening.

// Announce tells the gateways of db's org that listen on channel about subject.
func (db *DB) Announce(ctx context.Context, channel, subject string) error {
	_, err := db.Pool.Exec(ctx, `SELECT pg_notify($1, $2)`, channel, db.Org+"/"+subject)
	return err
}

// Listen calls heard with each subject announced on channel in db's org until
// ctx ends. It holds a connection of its own, outside the pool. Each time it
// begins listening, including after a lost connection, it calls heard("") for
// whatever it may have missed.
func (db *DB) Listen(ctx context.Context, channel string, heard func(subject string)) {
	for {
		db.listen(ctx, channel, heard)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// ListenerName is the application_name of db's listening connection, by which
// pg_stat_activity shows it.
func (db *DB) ListenerName() string { return "forgetmenote listener " + db.Holder }

func (db *DB) listen(ctx context.Context, channel string, heard func(subject string)) {
	config := db.Pool.Config().ConnConfig
	config.RuntimeParams["application_name"] = db.ListenerName()
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return
	}
	defer func() {
		closing, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn.Close(closing)
	}()
	if _, err = conn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize()); err != nil {
		return
	}
	heard("")
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return
		}
		if subject, ok := strings.CutPrefix(n.Payload, db.Org+"/"); ok {
			heard(subject)
		}
	}
}
