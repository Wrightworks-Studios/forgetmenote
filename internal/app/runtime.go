package app

import (
	"context"
	"errors"
	"forgetmenote/internal/blob"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/gitexport"
	"forgetmenote/internal/modules"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/web"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

type Options struct {
	Data, Origin, Token string
	BrowseExcluded      []string
	// Database is the checked Postgres control plane. The runtime closes it.
	Database *pg.DB
	// Blobs keeps files and notebook content blocks outside the database, or
	// is nil for the data directory. The runtime closes it.
	Blobs blob.Store
	// Modules is the module set the instance runs, from instance.json's
	// modules.off; the zero set runs every module.
	Modules modules.Set
}

// Runtime owns an instance from store opening through request drain. Provider
// configuration is installed on Server before Serve recovers and admits work.
type Runtime struct {
	Server                    *web.Server
	Catalog                   *catalog.Catalog
	Notes                     *Notes
	Database                  *pg.DB
	Blobs                     blob.Store
	mu                        sync.Mutex
	started, admitted, closed bool
	cancel                    context.CancelFunc
	done                      chan struct{}
	closeOnce                 sync.Once
}

// Open opens the instance's notes store and catalog, then exports its
// notebooks in the background for backups. A catalog notebook the notes store
// lacks stops it, without changing the data directory. On failure it closes
// what it opened; the caller still closes Database and Blobs.
func Open(options Options) (*Runtime, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	r := &Runtime{Database: options.Database, Blobs: options.Blobs}
	var own io.Closer
	fail := func(err error) (*Runtime, error) {
		if r.Catalog != nil {
			r.Catalog.Close()
		}
		if r.Notes != nil {
			r.Notes.Close()
		}
		if own != nil {
			own.Close()
		}
		return nil, err
	}
	if r.Blobs == nil {
		if err := os.MkdirAll(options.Data, 0700); err != nil {
			return nil, err
		}
		dir, err := blob.OpenDir(options.Data)
		if err != nil {
			return nil, err
		}
		r.Blobs, own = dir, dir
	}
	var err error
	if r.Notes, err = OpenNotes(ctx, options.Database, r.Blobs); err != nil {
		return fail(err)
	}
	if r.Catalog, err = catalog.OpenWithNotes(options.Data, options.BrowseExcluded, options.Database, r.Notes.Store); err != nil {
		return fail(err)
	}
	if err = r.Notes.Export(options.Data, gitexport.Options{Notebooks: catalogNotebooks(r.Catalog)}); err != nil {
		return fail(err)
	}
	if r.Server, err = web.NewWithBlobs(r.Catalog, options.Database, r.Blobs, options.Modules, options.Data, options.Origin, options.Token); err != nil {
		return fail(err)
	}
	r.Notes.Exporter.Start()
	return r, nil
}
func (r *Runtime) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	admitted := r.admitted && !r.closed
	r.mu.Unlock()
	if !admitted {
		http.Error(w, "Instance is not accepting requests", http.StatusServiceUnavailable)
		return
	}
	r.Server.ServeHTTP(w, req)
}
func (r *Runtime) Serve(ctx context.Context, server *http.Server, timeout time.Duration) error {
	r.mu.Lock()
	if r.started || r.closed {
		r.mu.Unlock()
		return errors.New("instance runtime is already started or closed")
	}
	r.started = true
	r.done = make(chan struct{})
	ctx, r.cancel = context.WithCancel(ctx)
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.admitted = false; r.mu.Unlock(); close(r.done) }()
	if err := r.Server.RecoverAuthority(ctx); err != nil {
		return err
	}
	if err := r.Server.Start(ctx); err != nil {
		return err
	}
	r.mu.Lock()
	r.admitted = !r.closed
	server.Handler = r
	r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return Serve(ctx, server, timeout)
}
func (r *Runtime) Close() {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		r.admitted = false
		if r.cancel != nil {
			r.cancel()
		}
		done := r.done
		r.mu.Unlock()
		if done != nil {
			<-done
		}
		r.Server.Close()
		r.Catalog.Close()
		r.Notes.Close()
		if r.Database != nil {
			r.Database.Close()
		}
		if closer, ok := r.Blobs.(io.Closer); ok {
			closer.Close()
		}
	})
}
