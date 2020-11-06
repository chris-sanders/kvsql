package server

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"time"
	
	"github.com/canonical/go-dqlite"
	"github.com/canonical/go-dqlite/app"
	"github.com/canonical/go-dqlite/client"
	"github.com/devec0/kvsql/server/config"
	"github.com/ghodss/yaml"
	"github.com/pkg/errors"
	"github.com/devec0/kine/pkg/endpoint"
	log "k8s.io/klog/v2"
)

// Server sets up a single dqlite node and serves the cluster management API.
type Server struct {
	dir        string // Data directory
	address    string // Network address
	app        *app.App
	cancelKine context.CancelFunc
}

func New(dir string) (*Server, error) {

	log.Info("Creating new kvsql instance")
	// Check if we're initializing a new node (i.e. there's an init.yaml).
	cfg, err := config.Load(dir)
	if err != nil {
		return nil, err
	}

	log.Info("Loaded configuration for kvsql/kine/dqline node")
	if cfg.Update != nil {
		info := client.NodeInfo{}
		path := filepath.Join(dir, "info.yaml")
		data, err := ioutil.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(data, &info); err != nil {
			return nil, err
		}
		log.Infof("kvsql setting dqlite addresses: %v", info)
		info.Address = cfg.Update.Address
		data, err = yaml.Marshal(info)
		if err != nil {
			return nil, err
		}
		if err := ioutil.WriteFile(path, data, 0600); err != nil {
			return nil, err
		}
		log.Info("kvsql configuring dqlite membership")
		nodes := []dqlite.NodeInfo{info}
		if err := dqlite.ReconfigureMembership(dir, nodes); err != nil {
			return nil, err
		}
		log.Info("kvsql configuring dqlite YAML store")
		store, err := client.NewYamlNodeStore(filepath.Join(dir, "cluster.yaml"))
		if err != nil {
			return nil, err
		}
		log.Info("kvsql setting dqlite datastore")
		if err := store.Set(context.Background(), nodes); err != nil {
			return nil, err
		}
		if err := os.Remove(filepath.Join(dir, "update.yaml")); err != nil {
			return nil, errors.Wrap(err, "remove update.yaml")
		}
	}

	options := []app.Option{
		app.WithTLS(app.SimpleTLSConfig(cfg.KeyPair, cfg.Pool)),
		app.WithFailureDomain(cfg.FailureDomain),
	}

	// Possibly initialize our ID, address and initial node store content.
	if cfg.Init != nil {
		log.Info("Attempting to initialise the kvsql/kine/dqline cluster")
		options = append(options, app.WithAddress(cfg.Init.Address), app.WithCluster(cfg.Init.Cluster))
	}

	log.Info("kvsql booting dqlite app")
	app, err := app.New(dir, options...)
	if err != nil {
		return nil, err
	}
	if cfg.Init != nil {
		log.Info("Removing kvsql cluster init.yaml now that we are initialised")
		if err := os.Remove(filepath.Join(dir, "init.yaml")); err != nil {
			return nil, err
		}
	}

	log.Info("kvsql creating context and deferring")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if err := app.Ready(ctx); err != nil {
		log.Info("kvsql error on ready context: %v", err)
		return nil, err
	}

	log.Info("kvsql configuring dqlite/kine endpoints")
	peers := filepath.Join(dir, "cluster.yaml")
	config := endpoint.Config{
	        Listener: "tcp://127.0.0.1:12379",
		Endpoint: fmt.Sprintf("dqlite://k8s?peer-file=%s&driver-name=%s", peers, app.Driver()),
	}
	
	kineCtx, cancelKine := context.WithCancel(context.Background())
	log.Infof("Starting kine listener on %s", config.Listener)
	
	if _, err := endpoint.Listen(kineCtx, config); err != nil {
		log.Infof("Kine context exited in error: %v", err)
		<-kineCtx.Done()
		return nil, errors.Wrap(err, "kine")
	}

	s := &Server{
		dir:        dir,
		address:    cfg.Address,
		app:        app,
		cancelKine: cancelKine,
	}

	log.Infof("kvsql returning new server instance: %s", cfg.Address)
	return s, nil
}

func (s *Server) Close(ctx context.Context) error {
	if s.cancelKine != nil {
		log.Info("Cancelling kine context")
		s.cancelKine()
	}
	s.app.Handover(ctx)
	if err := s.app.Close(); err != nil {
		log.Info("Stopped dqlite and requested handover")
		return errors.Wrap(err, "stop dqlite app")
	}
	return nil
}
