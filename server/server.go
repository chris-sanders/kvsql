package server

import (
	"context"
	"crypto/tls"
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"github.com/canonical/go-dqlite"
	"github.com/canonical/go-dqlite/client"
	"github.com/canonical/go-dqlite/driver"
	"github.com/pkg/errors"
)

// Server sets up a single dqlite node and serves the cluster management API.
type Server struct {
	dir  string       // Data directory
	api  *http.Server // API server
	node *dqlite.Node // Dqlite node
	db   *sql.DB      // Database connection
}

func New(dir string) (*Server, error) {
	// Check if we're initializing a new node (i.e. there's an init.yaml).
	init, err := maybeLoadInit(dir)
	if err != nil {
		return nil, err
	}

	// Open the node store, effectively creating a new empty one if we're
	// initializing.
	store, err := client.DefaultNodeStore(filepath.Join(dir, "servers.sql"))
	if err != nil {
		return nil, errors.Wrap(err, "open node store")
	}

	info := dqlite.NodeInfo{}
	if init != nil {
		info.Address = init.Address
		servers := []client.NodeInfo{}
		if len(init.Cluster) == 0 {
			// This is the first node of a new cluster.
			info.ID = 1
			servers = append(servers, info)
		}
		if err := writeInfo(dir, info); err != nil {
			return nil, err
		}
		if err := rmInit(dir); err != nil {
			return nil, err
		}
		if err := store.Set(context.Background(), []client.NodeInfo{info}); err != nil {
			return nil, errors.Wrap(err, "initialize node store")
		}
	}

	cfg, err := newTLSServerConfig(dir)
	if err != nil {
		return nil, err
	}

	listener, err := tls.Listen("tcp", info.Address, cfg)
	if err != nil {
		return nil, errors.Wrap(err, "bind API address")
	}

	dial, err := makeDqliteDialFunc(dir)
	if err != nil {
		return nil, err
	}

	node, err := dqlite.New(
		info.ID, info.Address, dir, dqlite.WithBindAddress("@"), dqlite.WithDialFunc(dial))
	if err != nil {
		return nil, errors.Wrap(err, "create dqlite node")
	}
	if err := node.Start(); err != nil {
		return nil, errors.Wrap(err, "start dqlite node")
	}

	conns := make(chan net.Conn)

	proxy := &dqliteProxy{conns: conns, addr: node.BindAddress()}
	go proxy.Start()

	mux := http.NewServeMux()
	mux.HandleFunc("/dqlite", makeDqliteHandler(conns))
	api := &http.Server{Handler: mux}

	go func() {
		if err := api.Serve(listener); err != http.ErrServerClosed {
			panic(err)
		}
		close(conns)
	}()

	driver, err := driver.New(
		store, driver.WithDialFunc(dial),
		driver.WithConnectionTimeout(10*time.Second),
		driver.WithContextTimeout(10*time.Second),
	)
	if err != nil {
		return nil, errors.Wrap(err, "create dqlite driver")
	}
	name := fmt.Sprintf("dqlite-%d", info.ID)
	sql.Register(name, driver)

	db, err := sql.Open(name, "k8s")
	if err != nil {
		return nil, errors.Wrap(err, "open cluster database")
	}

	// If we are initializing a new node, update the cluster database
	// accordingly.
	if init != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if len(init.Cluster) == 0 {
			if err := createServersTable(ctx, db); err != nil {
				return nil, err
			}
		}

		if err := insertServer(ctx, db, info); err != nil {
			return nil, err
		}
	}

	s := &Server{
		dir:  dir,
		api:  api,
		node: node,
		db:   db,
	}

	return s, nil
}

func (s *Server) Close(ctx context.Context) error {
	if err := s.db.Close(); err != nil {
		return errors.Wrap(err, "close cluster database")
	}
	if err := s.api.Shutdown(ctx); err != nil {
		return errors.Wrap(err, "shutdown API server")
	}
	if err := s.node.Close(); err != nil {
		return errors.Wrap(err, "stop dqlite node")
	}
	return nil
}
