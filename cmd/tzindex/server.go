// Copyright (c) 2020-2021 Blockwatch Data Inc.
// Author: alex@blockwatch.cc

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"blockwatch.cc/packdb/pack"
	"blockwatch.cc/packdb/store"
	"blockwatch.cc/tzindex/etl"
	"blockwatch.cc/tzindex/etl/metadata"
	"blockwatch.cc/tzindex/rpc"
	"blockwatch.cc/tzindex/server"
	"github.com/echa/config"
)

func runServer() error {
	// set user agent in library client
	server.UserAgent = UserAgent()
	server.ApiVersion = apiVersion
	pack.QueryLogMinDuration = config.GetDuration("db.log_slow_queries")

	// load metadata extensions
	if err := metadata.LoadExtensions(); err != nil {
		return err
	}

	engine := config.GetString("db.engine")
	pathname := config.GetString("db.path")
	log.Infof("Using %s database %s", engine, pathname)
	if unsafe {
		log.Warnf("Enabled NOSYNC mode. Database will not be safe on crashes!")
	}

	// make sure paths exist
	if err := os.MkdirAll(pathname, 0700); err != nil {
		return err
	}

	if snapPath := config.GetString("crawler.snapshot_path"); snapPath != "" {
		if err := os.MkdirAll(snapPath, 0700); err != nil {
			return err
		}
	}

	// open shared state database
	statedb, err := store.Open(engine, filepath.Join(pathname, etl.StateDBName), DBOpts(engine, false, unsafe))
	if err != nil {
		if !store.IsError(err, store.ErrDbDoesNotExist) {
			return fmt.Errorf("error opening %s database: %v", etl.StateDBName, err)
		}
		statedb, err = store.Create(engine, filepath.Join(pathname, etl.StateDBName), DBOpts(engine, false, unsafe))
		if err != nil {
			return fmt.Errorf("error creating %s database: %v", etl.StateDBName, err)
		}
	}
	defer statedb.Close()

	// open RPC client when requested
	var rpcclient *rpc.Client
	if !norpc {
		rpcclient, err = newRPCClient()
		if err != nil {
			return err
		}
	} else {
		noindex = true
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// enable index storage tables
	indexer := etl.NewIndexer(etl.IndexerConfig{
		DBPath:    pathname,
		DBOpts:    DBOpts(engine, false, unsafe),
		StateDB:   statedb,
		Indexes:   enabledIndexes(),
		LightMode: lightIndex,
	})
	defer indexer.Close()

	crawler := etl.NewCrawler(etl.CrawlerConfig{
		DB:            statedb,
		Indexer:       indexer,
		Client:        rpcclient,
		CacheSizeLog2: config.GetInt("crawler.cache_size_log2"),
		Queue:         config.GetInt("crawler.queue"),
		Delay:         config.GetInt("crawler.delay"),
		EnableMonitor: !nomonitor,
		StopBlock:     stop,
		Validate:      validate,
		Snapshot: &etl.SnapshotConfig{
			Path:          config.GetString("crawler.snapshot_path"),
			Blocks:        config.GetInt64Slice("crawler.snapshot_blocks"),
			BlockInterval: config.GetInt64("crawler.snapshot_interval"),
		},
	})
	// not indexing means we do not auto-index, but allow access to
	// existing indexes
	if !noindex {
		if err := crawler.Init(ctx, etl.MODE_SYNC); err != nil {
			return fmt.Errorf("error initializing crawler: %v", err)
		}
		crawler.Start()
		defer crawler.Stop(ctx)
	} else {
		if err := crawler.Init(ctx, etl.MODE_INFO); err != nil {
			return fmt.Errorf("error initializing crawler: %v", err)
		}
	}

	// setup HTTP server
	if !noapi {
		srv, err := server.New(&server.Config{
			Crawler: crawler,
			Indexer: indexer,
			Client:  rpcclient,
			Http: server.HttpConfig{
				Addr:                config.GetString("server.addr"),
				Port:                config.GetInt("server.port"),
				MaxWorkers:          config.GetInt("server.workers"),
				MaxQueue:            config.GetInt("server.queue"),
				ReadTimeout:         config.GetDuration("server.read_timeout"),
				HeaderTimeout:       config.GetDuration("server.header_timeout"),
				WriteTimeout:        config.GetDuration("server.write_timeout"),
				KeepAlive:           config.GetDuration("server.keepalive"),
				ShutdownTimeout:     config.GetDuration("server.shutdown_timeout"),
				DefaultListCount:    config.GetUint("server.default_list_count"),
				MaxListCount:        config.GetUint("server.max_list_count"),
				DefaultExploreCount: config.GetUint("server.default_explore_count"),
				MaxExploreCount:     config.GetUint("server.max_explore_count"),
				CorsEnable:          cors || config.GetBool("server.cors_enable"),
				CorsOrigin:          config.GetString("server.cors_origin"),
				CorsAllowHeaders:    config.GetString("server.cors_allow_headers"),
				CorsExposeHeaders:   config.GetString("server.cors_expose_headers"),
				CorsMethods:         config.GetString("server.cors_methods"),
				CorsMaxAge:          config.GetString("server.cors_maxage"),
				CorsCredentials:     config.GetString("server.cors_credentials"),
				CacheEnable:         config.GetBool("server.cache_enable"),
				CacheControl:        config.GetString("server.cache_control"),
				CacheExpires:        config.GetDuration("server.cache_expires"),
				CacheMaxExpires:     config.GetDuration("server.cache_max"),
				MaxSeriesDuration:   config.GetDuration("server.max_series_duration"),
			},
		})
		if err != nil {
			return err
		}
		srv.Start()
		// drain connections, reject new connections
		defer srv.Stop()
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT,
	)
	<-c
	signal.Stop(c)
	return nil
}
