package server

import (
	"admin"
	"api/graphite"
	"api/http"
	"api/udp"
	"cluster"
	"configuration"
	"coordinator"
	"datastore"
	"runtime"
	"time"
	"wal"

	log "code.google.com/p/log4go"
	influxdb "github.com/influxdb/influxdb-go"
)

type Server struct {
	RaftServer     *coordinator.RaftServer
	ProtobufServer *coordinator.ProtobufServer
	ClusterConfig  *cluster.ClusterConfiguration
	HttpApi        *http.HttpServer
	GraphiteApi    *graphite.Server
	UdpApi         *udp.Server
	UdpServers     []*udp.Server
	AdminServer    *admin.HttpServer
	Coordinator    coordinator.Coordinator
	Config         *configuration.Configuration
	RequestHandler *coordinator.ProtobufRequestHandler
	stopped        bool
	writeLog       *wal.WAL
	shardStore     *datastore.ShardDatastore
}

func NewServer(config *configuration.Configuration) (*Server, error) {
	log.Info("Opening database at %s", config.DataDir)
	shardDb, err := datastore.NewShardDatastore(config)
	if err != nil {
		return nil, err
	}

	newClient := func(connectString string) cluster.ServerConnection {
		return coordinator.NewProtobufClient(connectString, config.ProtobufTimeout.Duration)
	}
	writeLog, err := wal.NewWAL(config)
	if err != nil {
		return nil, err
	}

	clusterConfig := cluster.NewClusterConfiguration(config, writeLog, shardDb, newClient)
	raftServer := coordinator.NewRaftServer(config, clusterConfig)
	clusterConfig.LocalRaftName = raftServer.GetRaftName()
	clusterConfig.SetShardCreator(raftServer)
	clusterConfig.CreateFutureShardsAutomaticallyBeforeTimeComes()

	coord := coordinator.NewCoordinatorImpl(config, raftServer, clusterConfig)
	requestHandler := coordinator.NewProtobufRequestHandler(coord, clusterConfig)
	protobufServer := coordinator.NewProtobufServer(config.ProtobufListenString(), requestHandler)

	raftServer.AssignCoordinator(coord)
	httpApi := http.NewHttpServer(config.ApiHttpPortString(), config.ApiReadTimeout, config.AdminAssetsDir, coord, coord, clusterConfig, raftServer)
	httpApi.EnableSsl(config.ApiHttpSslPortString(), config.ApiHttpCertPath)
	graphiteApi := graphite.NewServer(config, coord, clusterConfig)
	adminServer := admin.NewHttpServer(config.AdminAssetsDir, config.AdminHttpPortString())

	return &Server{
		RaftServer:     raftServer,
		ProtobufServer: protobufServer,
		ClusterConfig:  clusterConfig,
		HttpApi:        httpApi,
		GraphiteApi:    graphiteApi,
		Coordinator:    coord,
		AdminServer:    adminServer,
		Config:         config,
		RequestHandler: requestHandler,
		writeLog:       writeLog,
		shardStore:     shardDb}, nil
}

func (self *Server) ListenAndServe() error {
	err := self.RaftServer.ListenAndServe()
	if err != nil {
		return err
	}

	log.Info("Waiting for local server to be added")
	self.ClusterConfig.WaitForLocalServerLoaded()
	self.writeLog.SetServerId(self.ClusterConfig.ServerId())

	time.Sleep(5 * time.Second)

	// check to make sure that the raft connection string hasn't changed
	raftConnectionString := self.Config.RaftConnectionString()
	if self.ClusterConfig.LocalServer.ProtobufConnectionString != self.Config.ProtobufConnectionString() ||
		self.ClusterConfig.LocalServer.RaftConnectionString != raftConnectionString {

		log.Info("Sending change connection string command (%s,%s) (%s,%s)",
			self.ClusterConfig.LocalServer.ProtobufConnectionString,
			self.Config.ProtobufConnectionString(),
			self.ClusterConfig.LocalServer.RaftConnectionString,
			raftConnectionString,
		)

		err := self.RaftServer.ChangeConnectionString(
			self.ClusterConfig.LocalRaftName,
			self.Config.ProtobufConnectionString(),
			self.Config.RaftConnectionString(),
			true, // force the rename
		)
		if err != nil {
			panic(err)
		}
		log.Info("Connection string changed successfully")
	}

	go self.ProtobufServer.ListenAndServe()

	log.Info("Recovering from log...")
	err = self.ClusterConfig.RecoverFromWAL()
	if err != nil {
		return err
	}
	log.Info("recovered")

	err = self.Coordinator.(*coordinator.CoordinatorImpl).ConnectToProtobufServers(self.RaftServer.GetRaftName())
	if err != nil {
		return err
	}
	log.Info("Starting admin interface on port %d", self.Config.AdminHttpPort)
	go self.AdminServer.ListenAndServe()
	if self.Config.GraphiteEnabled {
		if self.Config.GraphitePort <= 0 || self.Config.GraphiteDatabase == "" {
			log.Warn("Cannot start graphite server. please check your configuration")
		} else {
			log.Info("Starting Graphite Listener on port %d", self.Config.GraphitePort)
			go self.GraphiteApi.ListenAndServe()
		}
	}

	// UDP input
	for _, udpInput := range self.Config.UdpServers {
		port := udpInput.Port
		database := udpInput.Database

		if port <= 0 {
			log.Warn("Cannot start udp server on port %d. please check your configuration", port)
			continue
		} else if database == "" {
			log.Warn("Cannot start udp server for database=\"\".  please check your configuration")
		}

		log.Info("Starting UDP Listener on port %d to database %s", port, database)

		addr := self.Config.UdpInputPortString(port)

		server := udp.NewServer(addr, database, self.Coordinator, self.ClusterConfig)
		self.UdpServers = append(self.UdpServers, server)
		go server.ListenAndServe()
	}

	log.Debug("ReportingDisabled: %s", self.Config.ReportingDisabled)
	if !self.Config.ReportingDisabled {
		go self.startReportingLoop()
	}

	// start processing continuous queries
	self.RaftServer.StartProcessingContinuousQueries()

	log.Info("Starting Http Api server on port %d", self.Config.ApiHttpPort)
	self.HttpApi.ListenAndServe()

	return nil
}

func (self *Server) startReportingLoop() chan struct{} {
	log.Debug("Starting Reporting Loop")
	self.reportStats()

	ticker := time.NewTicker(24 * time.Hour)
	for {
		select {
		case <-ticker.C:
			self.reportStats()
		}
	}
}

func (self *Server) reportStats() {
	client, err := influxdb.NewClient(&influxdb.ClientConfig{
		Database: "reporting",
		Host:     "m.influxdb.com:8086",
		Username: "reporter",
		Password: "influxdb",
	})

	if err != nil {
		log.Error("Couldn't create client for reporting: %s", err)
	} else {
		series := &influxdb.Series{
			Name:    "reports",
			Columns: []string{"os", "arch", "id", "version"},
			Points: [][]interface{}{
				{runtime.GOOS, runtime.GOARCH, self.RaftServer.GetRaftName(), self.Config.InfluxDBVersion},
			},
		}

		log.Info("Reporting stats: %#v", series)
		client.WriteSeries([]*influxdb.Series{series})
	}
}

func (self *Server) Stop() {
	if self.stopped {
		return
	}
	log.Info("Stopping server")
	self.stopped = true

	log.Info("Stopping api server")
	self.HttpApi.Close()
	log.Info("Api server stopped")

	log.Info("Stopping admin server")
	self.AdminServer.Close()
	log.Info("admin server stopped")

	log.Info("Stopping raft server")
	self.RaftServer.Close()
	log.Info("Raft server stopped")

	log.Info("Stopping protobuf server")
	self.ProtobufServer.Close()
	log.Info("protobuf server stopped")

	log.Info("Stopping wal")
	self.writeLog.Close()
	log.Info("wal stopped")

	log.Info("Stopping shard store")
	self.shardStore.Close()
	log.Info("shard store stopped")
}
