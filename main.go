package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/context"
	"google.golang.org/grpc"

	"github.com/goburrow/cache"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/testdata"

	"github.com/bgokden/veri/data"
	pb "github.com/bgokden/veri/veriservice"
	"github.com/segmentio/ksuid"

	"github.com/gorilla/mux"
)

var (
	tls        = flag.Bool("tls", false, "Connection uses TLS if true, else plain TCP")
	certFile   = flag.String("cert_file", "", "The TLS cert file")
	keyFile    = flag.String("key_file", "", "The TLS key file")
	jsonDBFile = flag.String("json_db_file", "testdata/route_guide_db.json", "A json file containing a list of features")
	port       = flag.Int("port", 10000, "The server port")
	services   = flag.String("services", "", "Comma separated list of services")
	evictable  = flag.Bool("evictable", true, "Data is evicted by time if it is true")
)

// This is set in compile time for optimization
const k = 1024 // 1024

// 0 => euclidean distance
// 1 => consine distance
const distance_mode = 0

type Peer struct {
	address   string
	version   string
	avg       []float64
	hist      []float64
	n         int64
	timestamp int64
}

type Client struct {
	address string
	client  *pb.VeriServiceClient
	conn    *grpc.ClientConn
}

type veriServiceServer struct {
	k int64
	// d           int64
	// avg         []float64
	// n           int64
	// maxDistance float64
	// hist        []float64
	address   string
	version   string
	timestamp int64
	// dirty                 bool
	// latestNumberOfInserts int
	state        int
	maxMemoryMiB uint64
	// averageTimestamp int64
	// pointsMap             sync.Map
	services   sync.Map
	peers      sync.Map
	knnQueryId cache.Cache
	// pointsMu              sync.RWMutex // protects points
	// treeMu                sync.RWMutex // protects KDTree
	// tree                  *kdtree.KDTree
	cache   cache.Cache
	clients sync.Map
	dt      *data.Data
}

func getCurrentTime() int64 {
	return time.Now().Unix()
}

func (s *veriServiceServer) GetLocalData(rect *pb.GetLocalDataRequest, stream pb.VeriService_GetLocalDataServer) error {
	return s.dt.GetAll(stream)
}

func (s *veriServiceServer) GetKnnFromPeer(in *pb.KnnRequest, peer *Peer, featuresChannel chan<- pb.Feature) {
	log.Printf("GetKnnFromPeer %s", peer.address)
	client, err0 := s.get_client(peer.address)
	if err0 == nil {
		grpc_client := client.client
		resp, err := (*grpc_client).GetKnnStream(context.Background(), in)
		if err != nil {
			log.Printf("There is an error: %v", err)
			// conn.Close()
			go s.refresh_client(peer.address)
			return
		}
		// if resp.Success {
		// log.Printf("A new Response has been received with id: %s", resp.Id)
		for {
			feature, err := resp.Recv()
			if err != nil {
				log.Printf("Error: (%v)", err)
				break
			}
			log.Printf("New Feature from Peer (%s) : %v", peer.address, feature.GetLabel())
			featuresChannel <- *(feature)
		}
		// conn.Close()
	}
}

func (s *veriServiceServer) GetKnnFromPeers(in *pb.KnnRequest, featuresChannel chan<- pb.Feature) {
	timeout := int64(float64(in.GetTimeout()) * 0.9)
	request := &pb.KnnRequest{
		Feature:   in.GetFeature(),
		Id:        in.GetId(),
		K:         in.GetK(),
		Timestamp: in.GetTimestamp(),
		Timeout:   timeout,
	}
	log.Printf("GetKnnFromPeers")
	s.peers.Range(func(key, value interface{}) bool {
		peerAddress := key.(string)
		log.Printf("Peer %s", peerAddress)
		if len(peerAddress) > 0 && peerAddress != s.address {
			peerValue := value.(Peer)
			s.GetKnnFromPeer(request, &peerValue, featuresChannel)
		}
		return true
	})
}

func (s *veriServiceServer) GetKnnFromLocal(in *pb.KnnRequest, featuresChannel chan<- pb.Feature) {
	log.Printf("GetKnnFromLocal")
	point := data.NewEuclideanPointArr(in.GetFeature())
	ans, err := s.dt.GetKnn(int64(in.GetK()), point)
	if err == nil {
		for i := 0; i < len(ans); i++ {
			// log.Printf("New Feature from Local before")
			feature := data.NewFeatureFromEuclideanPoint(ans[i])
			// log.Printf("New Feature from Local: %v after", feature.GetLabel())
			featuresChannel <- *feature
		}
	} else {
		log.Printf("Error in GetKnn: %v", err.Error())
	}
}

// Do a distributed Knn search
func (s *veriServiceServer) GetKnn(ctx context.Context, in *pb.KnnRequest) (*pb.KnnResponse, error) {
	request := *in
	d := int64(len(request.GetFeature()))
	var featureHash [k]float64
	copy(featureHash[:d], request.GetFeature()[:])
	if len(in.GetId()) == 0 {
		request.Id = ksuid.New().String()
		s.knnQueryId.Put(request.Id, true)
	} else {
		_, loaded := s.knnQueryId.GetIfPresent(request.GetId())
		if loaded {
			cachedResult, isCached := s.cache.GetIfPresent(featureHash)
			if isCached {
				log.Printf("Return cached result for id %v", request.GetId())
				return cachedResult.(*pb.KnnResponse), nil
			} else {
				log.Printf("Return un-cached result for id %v since it is already processed.", request.GetId())
				return &pb.KnnResponse{Id: in.Id, Features: nil}, nil
			}
		} else {
			s.knnQueryId.Put(request.GetId(), getCurrentTime())
		}
	}
	featuresChannel := make(chan pb.Feature, in.GetK())
	go s.GetKnnFromPeers(&request, featuresChannel)
	go s.GetKnnFromLocal(&request, featuresChannel)
	// time.Sleep(1 * time.Second)
	// close(featuresChannel)
	responseFeatures := make([]*pb.Feature, 0)
	dataAvailable := true
	timeLimit := time.After(time.Duration(in.GetTimeout()) * time.Millisecond)
	// reduceMap := make(map[data.EuclideanPointKey]data.EuclideanPointValue)
	reduceData := data.NewTempData()
	for dataAvailable {
		select {
		case feature := <-featuresChannel:
			key, value := data.FeatureToEuclideanPointKeyValue(&feature)
			// reduceMap[*key] = *value
			reduceData.Insert(*key, *value)
		case <-timeLimit:
			log.Printf("timeout")
			dataAvailable = false
			break
		}
	}
	point := data.NewEuclideanPointArr(in.Feature)
	reduceData.Process(true)
	ans, err := reduceData.GetKnn(int64(in.K), point)
	if err != nil {
		log.Printf("Error in Knn: %v", err.Error())
		return &pb.KnnResponse{Id: request.GetId(), Features: responseFeatures}, err
	}
	for i := 0; i < len(ans); i++ {
		featureJson := data.NewFeatureFromPoint(ans[i])
		// log.Printf("New Feature (Get Knn): %v", ans[i].GetLabel())
		responseFeatures = append(responseFeatures, featureJson)
	}
	s.knnQueryId.Put(request.GetId(), true)
	s.cache.Put(featureHash, &pb.KnnResponse{Id: request.GetId(), Features: responseFeatures})
	return &pb.KnnResponse{Id: request.GetId(), Features: responseFeatures}, nil
}

func (s *veriServiceServer) GetKnnStream(in *pb.KnnRequest, stream pb.VeriService_GetKnnStreamServer) error {
	request := *in
	d := int64(len(request.GetFeature()))
	var featureHash [k]float64
	copy(featureHash[:d], request.GetFeature()[:])
	if len(in.GetId()) == 0 {
		request.Id = ksuid.New().String()
		s.knnQueryId.Put(request.Id, true)
	} else {
		_, loaded := s.knnQueryId.GetIfPresent(request.GetId())
		if loaded {
			cachedResult, isCached := s.cache.GetIfPresent(featureHash)
			if isCached {
				log.Printf("Return cached result for id %v", request.GetId())
				result := cachedResult.(*pb.KnnResponse).GetFeatures()
				for _, e := range result {
					stream.Send(e)
				}
				return nil
			} else {
				log.Printf("Return un-cached result for id %v since it is already processed.", request.GetId())
				return nil
			}
		} else {
			s.knnQueryId.Put(request.GetId(), getCurrentTime())
		}
	}
	featuresChannel := make(chan pb.Feature, in.GetK())
	go s.GetKnnFromPeers(&request, featuresChannel)
	go s.GetKnnFromLocal(&request, featuresChannel)
	// time.Sleep(1 * time.Second)
	// close(featuresChannel)
	responseFeatures := make([]*pb.Feature, 0)
	dataAvailable := true
	timeLimit := time.After(time.Duration(in.GetTimeout()) * time.Millisecond)
	// reduceMap := make(map[data.EuclideanPointKey]data.EuclideanPointValue)
	reduceData := data.NewTempData()
	for dataAvailable {
		select {
		case feature := <-featuresChannel:
			key, value := data.FeatureToEuclideanPointKeyValue(&feature)
			// reduceMap[*key] = *value
			reduceData.Insert(*key, *value)
		case <-timeLimit:
			log.Printf("timeout")
			dataAvailable = false
			break
		}
	}
	point := data.NewEuclideanPointArr(in.Feature)
	reduceData.Process(true)
	ans, err := reduceData.GetKnn(int64(in.K), point)
	if err != nil {
		log.Printf("Error in Knn: %v", err.Error())
		return err
	}
	for i := 0; i < len(ans); i++ {
		feature := data.NewFeatureFromPoint(ans[i])
		// log.Printf("New Feature (Get Knn): %v", ans[i].GetLabel())
		stream.Send(feature)
		responseFeatures = append(responseFeatures, feature)
	}
	s.knnQueryId.Put(request.GetId(), true)
	s.cache.Put(featureHash, &pb.KnnResponse{Id: request.GetId(), Features: responseFeatures})
	return nil
}

func (s *veriServiceServer) Insert(ctx context.Context, in *pb.InsertionRequest) (*pb.InsertionResponse, error) {
	if s.state > 2 {
		return &pb.InsertionResponse{Code: 1}, nil
	}
	key, value := data.InsertionRequestToEuclideanPointKeyValue(in)
	s.dt.Insert(*key, *value)
	return &pb.InsertionResponse{Code: 0}, nil
}

func (s *veriServiceServer) InsertStream(stream pb.VeriService_InsertStreamServer) error {
	for {
		in, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			log.Fatalf("Failed to receive a note : %v", err)
		}
		key, value := data.FeatureToEuclideanPointKeyValue(in)
		s.dt.Insert(*key, *value)

		if s.state > 2 {
			stream.Send(&pb.InsertionResponse{Code: 1})
			return nil
		} else {
			stream.Send(&pb.InsertionResponse{Code: 0})
		}
	}
	return nil
}

func (s *veriServiceServer) Join(ctx context.Context, in *pb.JoinRequest) (*pb.JoinResponse, error) {
	// log.Printf("Join request received %v", *in)
	p, _ := peer.FromContext(ctx)
	address := strings.Split(p.Addr.String(), ":")[0] + ":" + strconv.FormatInt(int64(in.GetPort()), 10)
	// log.Printf("Peer with Addr: %s called Join", address)
	peer := Peer{
		address:   address,
		avg:       in.GetAvg(),
		version:   in.GetVersion(),
		hist:      in.GetHist(),
		n:         in.GetN(),
		timestamp: in.GetTimestamp(),
	}
	s.peers.Store(address, peer)
	return &pb.JoinResponse{Address: address}, nil
}

func (s *veriServiceServer) ExchangeServices(ctx context.Context, in *pb.ServiceMessage) (*pb.ServiceMessage, error) {
	inputServiceList := in.GetServices()
	for i := 0; i < len(inputServiceList); i++ {
		s.services.Store(inputServiceList[i], true)
	}
	outputServiceList := make([]string, 0)
	s.services.Range(func(key, value interface{}) bool {
		serviceName := key.(string)
		outputServiceList = append(outputServiceList, serviceName)
		return true
	})
	return &pb.ServiceMessage{Services: outputServiceList}, nil
}

func (s *veriServiceServer) ExchangePeers(ctx context.Context, in *pb.PeerMessage) (*pb.PeerMessage, error) {
	inputPeerList := in.GetPeers()
	for i := 0; i < len(inputPeerList); i++ {
		insert := true
		temp, ok := s.peers.Load(inputPeerList[i].GetAddress())
		if ok {
			peerOld := temp.(Peer)
			if peerOld.timestamp > inputPeerList[i].GetTimestamp() || inputPeerList[i].GetTimestamp()+300 < getCurrentTime() {
				insert = false
			}
		}
		if insert {
			peer := Peer{
				address:   inputPeerList[i].GetAddress(),
				version:   inputPeerList[i].GetVersion(),
				avg:       inputPeerList[i].GetAvg(),
				hist:      inputPeerList[i].GetHist(),
				n:         inputPeerList[i].GetN(),
				timestamp: inputPeerList[i].GetTimestamp(),
			}
			s.peers.Store(inputPeerList[i].GetAddress(), peer)
		}
	}
	outputPeerList := make([]*pb.Peer, 0)
	s.peers.Range(func(key, value interface{}) bool {
		// address := key.(string)
		peer := value.(Peer)
		if peer.timestamp+300 > getCurrentTime() {
			peerProto := &pb.Peer{
				Address:   peer.address,
				Version:   peer.version,
				Avg:       peer.avg,
				Hist:      peer.hist,
				N:         peer.n,
				Timestamp: peer.timestamp,
			}
			outputPeerList = append(outputPeerList, peerProto)
		}
		return true
	})
	return &pb.PeerMessage{Peers: outputPeerList}, nil
}

func (s *veriServiceServer) getClient(address string) (*pb.VeriServiceClient, *grpc.ClientConn, error) {
	conn, err := grpc.Dial(address, grpc.WithInsecure())
	if err != nil {
		log.Printf("fail to dial: %v", err)
		return nil, nil, err
	}
	client := pb.NewVeriServiceClient(conn)
	return &client, conn, nil
}

func (s *veriServiceServer) new_client(address string) (*Client, error) {
	client, conn, err := s.getClient(address)
	if err != nil {
		log.Printf("fail to create a client: %v", err)
		return nil, err
	}
	return &Client{
		address: address,
		client:  client,
		conn:    conn,
	}, nil
}

/*
There may be some concurrency problems where unclosed connections can occur
*/
func (s *veriServiceServer) get_client(address string) (Client, error) {
	client, ok := s.clients.Load(address)
	if ok {
		return (*(client.(*Client))), nil
	} else {
		new_client, err := s.new_client(address)
		if err != nil {
			return Client{}, err
		} else {
			s.clients.Store(address, new_client)
			return (*new_client), nil
		}
	}
	return Client{}, errors.New("Can not initilize client")
}

func (s *veriServiceServer) refresh_client(address string) {
	log.Printf("Renewing client with address %v", address)
	new_client, err := s.new_client(address)
	if err != nil {
		log.Printf("fail to get a client: %v", err) // this is probably really bad
	} else {
		s.clients.Store(address, new_client)
	}
}

func (s *veriServiceServer) callJoin(client *pb.VeriServiceClient) {
	stats := s.dt.GetStats()
	request := &pb.JoinRequest{
		Address:   s.address,
		Avg:       stats.Avg,
		Port:      int32(*port),
		Version:   s.version,
		Hist:      stats.Hist,
		N:         stats.N,
		Timestamp: s.timestamp,
	}
	// log.Printf("Call Join Request %v", *request)
	resp, err := (*client).Join(context.Background(), request)
	if err != nil {
		log.Printf("(Call Join) There is an error %v", err)
		return
	}
	if s.address != resp.GetAddress() {
		s.address = resp.GetAddress()
	}
}

func (s *veriServiceServer) callExchangeServices(client *pb.VeriServiceClient) {
	outputServiceList := make([]string, 0)
	s.services.Range(func(key, value interface{}) bool {
		serviceName := key.(string)
		outputServiceList = append(outputServiceList, serviceName)
		return true
	})
	request := &pb.ServiceMessage{
		Services: outputServiceList,
	}
	resp, err := (*client).ExchangeServices(context.Background(), request)
	if err != nil {
		log.Printf("(callExchangeServices) There is an error %v", err)
		return
	}
	inputServiceList := resp.GetServices()
	for i := 0; i < len(inputServiceList); i++ {
		s.services.Store(inputServiceList[i], true)
	}
	// log.Printf("Services exhanged")
}

func (s *veriServiceServer) callExchangeData(client *pb.VeriServiceClient, peer *Peer) {
	// need to check avg and hist differences ...
	// chose datum
	stats := s.dt.GetStats()
	log.Printf("For peer: %v s.n: %d peer.n: %d peer timestamp: %v currentTime: %v", peer.address, stats.N, peer.n, peer.timestamp, getCurrentTime())
	if peer.timestamp+360 < getCurrentTime() {
		log.Printf("Peer data is too old, maybe peer is dead: %s, peer timestamp: %d, current time: %d", peer.address, peer.timestamp, getCurrentTime())
		// Maybe remove the peer here
		s.peers.Delete(peer.address)
		s.clients.Delete(peer.address) // maybe try closing before delete
		return
	}
	if peer.timestamp+30 < getCurrentTime() && s.state == 0 {
		// log.Printf("Peer data is too old: %s", peer.address)
		// limit = 1 // no change can be risky
		return
	}
	if stats.N < peer.n {
		// log.Printf("Other peer should initiate exchange data %s", peer.address)
		return
	}
	distanceAvg := data.VectorDistance(stats.Avg, peer.avg)
	distanceHist := data.VectorDistance(stats.Hist, peer.hist)
	// log.Printf("%s => distanceAvg %f, distanceHist: %f", peer.address, distanceAvg, distanceHist)
	limit := int(((stats.N - peer.n) / 10) % 1000)
	nRatio := 0.0
	if peer.n != 0 {
		nRatio = float64(stats.N) / float64(peer.n)
	}
	if 0.99 < nRatio && nRatio < 1.01 && distanceAvg < 0.0005 && distanceHist < 0.0005 && s.state == 0 {
		// log.Printf("Decrease number of changes to 1 since stats are close enough %s", peer.address)
		limit = 1 // no change can be risky
	}
	points := s.dt.GetRandomPoints(limit)

	for _, point := range points {
		request := data.NewInsertionRequestFromPoint(point)
		resp, err := (*client).Insert(context.Background(), request)
		if err != nil {
			log.Printf("There is an error: %v", err)
		} else {
			// log.Printf("A new Response has been received for %d. with code: %d", i, resp.GetCode())
			if resp.GetCode() == 0 && s.state > 0 && rand.Float64() < (0.3*float64(s.state)) {
				key := data.NewEuclideanPointKeyFromPoint(point)
				s.dt.Delete(*key)
			}
		}
	}
}

func (s *veriServiceServer) callExchangePeers(client *pb.VeriServiceClient) {
	// log.Printf("callExchangePeers")
	outputPeerList := make([]*pb.Peer, 0)
	s.peers.Range(func(key, value interface{}) bool {
		// address := key.(string)
		peer := value.(Peer)
		peerProto := &pb.Peer{
			Address:   peer.address,
			Version:   peer.version,
			Avg:       peer.avg,
			Hist:      peer.hist,
			N:         peer.n,
			Timestamp: peer.timestamp,
		}
		outputPeerList = append(outputPeerList, peerProto)
		return true
	})
	request := &pb.PeerMessage{
		Peers: outputPeerList,
	}
	resp, err := (*client).ExchangePeers(context.Background(), request)
	if err != nil {
		log.Printf("(callExchangePeers) There is an error %v", err)
		return
	}
	inputPeerList := resp.GetPeers()
	for i := 0; i < len(inputPeerList); i++ {
		insert := true
		temp, ok := s.peers.Load(inputPeerList[i].GetAddress())
		if ok {
			peerOld := temp.(Peer)
			if peerOld.timestamp > inputPeerList[i].GetTimestamp() {
				insert = false
			}
		}
		if insert && s.address != inputPeerList[i].GetAddress() {
			peer := Peer{
				address:   inputPeerList[i].GetAddress(),
				version:   inputPeerList[i].GetVersion(),
				avg:       inputPeerList[i].GetAvg(),
				hist:      inputPeerList[i].GetHist(),
				n:         inputPeerList[i].GetN(),
				timestamp: inputPeerList[i].GetTimestamp(),
			}
			s.peers.Store(inputPeerList[i].GetAddress(), peer)
		}
	}
	// log.Printf("Peers exhanged")
}

func (s *veriServiceServer) SyncJoin() {
	// log.Printf("Sync Join")
	s.services.Range(func(key, value interface{}) bool {
		serviceName := key.(string)
		// log.Printf("Service %s", serviceName)
		if len(serviceName) > 0 {
			client, err := s.get_client(serviceName)
			if err == nil {
				grpc_client := client.client
				s.callJoin(grpc_client)
			} else {
				log.Printf("SyncJoin err: %v", err)
				go s.refresh_client(serviceName)
			}
			// conn.Close()
		}
		return true
	})
	// log.Printf("Service loop Ended")
	s.peers.Range(func(key, value interface{}) bool {
		peerAddress := key.(string)
		// log.Printf("Peer %s", peerAddress)
		if len(peerAddress) > 0 && peerAddress != s.address {
			peerValue := value.(Peer)
			client, err := s.get_client(peerAddress)
			if err == nil {
				grpc_client := client.client
				s.callJoin(grpc_client)
				s.callExchangeServices(grpc_client)
				s.callExchangePeers(grpc_client)
				s.callExchangeData(grpc_client, &peerValue)
			} else {
				log.Printf("SyncJoin 2 err: %v", err)
				go s.refresh_client(peerAddress)
			}
			// conn.Close()
		}
		return true
	})
	// log.Printf("Peer loop Ended")
}

func (s *veriServiceServer) isEvictable() bool {
	if s.state >= 2 && *evictable {
		return true
	}
	return false
}

func newServer() *veriServiceServer {
	s := &veriServiceServer{}
	s.dt = data.NewData()
	log.Printf("services %s", *services)
	serviceList := strings.Split(*services, ",")
	for _, service := range serviceList {
		if len(service) > 0 {
			s.services.Store(service, true)
		}
	}
	s.maxMemoryMiB = 1024
	s.timestamp = getCurrentTime()
	load := func(k cache.Key) (cache.Value, error) {
		return fmt.Sprintf("%d", k), nil
	}
	s.cache = cache.NewLoadingCache(load,
		cache.WithMaximumSize(1000),
		cache.WithExpireAfterAccess(10*time.Second),
		cache.WithRefreshAfterWrite(60*time.Second),
	)
	s.knnQueryId = cache.NewLoadingCache(load,
		cache.WithMaximumSize(1000),
		cache.WithExpireAfterAccess(10*time.Second),
		cache.WithRefreshAfterWrite(60*time.Second),
	)
	return s
}

func (s *veriServiceServer) check() {
	nextSyncJoinTime := getCurrentTime()
	for {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		// log.Printf("Alloc = %v MiB", bToMb(m.Alloc))
		// log.Printf("TotalAlloc = %v MiB", bToMb(m.TotalAlloc))
		// log.Printf("Sys = %v MiB", bToMb(m.Sys))
		// log.Printf("NumGC = %v\n", m.NumGC)
		currentMemory := float64(bToMb(m.Alloc))
		maxMemory := float64(s.maxMemoryMiB)
		if currentMemory < maxMemory*0.5 {
			s.state = 0 // Accept insert, don't delete while sending data
		} else if currentMemory < maxMemory*0.75 {
			s.state = 1 // Accept insert, delete while sending data
		} else if currentMemory < maxMemory*0.85 {
			s.state = 2 // Accept insert, delete while sending data and evict data
		} else {
			s.state = 3 // Don't accept insert, delete while sending data
		}
		// log.Printf("Current Memory = %f MiB => current State %d", currentMemory, s.state)
		// millisecondToSleep := int64(((s.latestNumberOfInserts + 100) % 1000) * 10)
		// log.Printf("millisecondToSleep: %d, len %d", millisecondToSleep, s.n)
		// time.Sleep(time.Duration(millisecondToSleep) * time.Millisecond)

		// currentTime := getCurrentTime()
		// log.Printf("Current Time: %v", currentTime)
		if nextSyncJoinTime <= getCurrentTime() {
			s.SyncJoin()
			nextSyncJoinTime = getCurrentTime() + 10
		}
		s.timestamp = getCurrentTime()
		time.Sleep(time.Duration(1000) * time.Millisecond) // always wait one second
	}
}

func bToMb(b uint64) uint64 {
	return b / 1024 / 1024
}

func GetHeath(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"alive": true}`)
}

func respondWithError(w http.ResponseWriter, code int, message string) {
	respondWithJSON(w, code, map[string]string{"error": message})
}

func respondWithJSON(w http.ResponseWriter, code int, payload interface{}) {
	response, _ := json.Marshal(payload)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(response)
}

func (s *veriServiceServer) restApi() {
	log.Println("Rest api stared")
	router := mux.NewRouter()
	router.HandleFunc("/", GetHeath).Methods("GET")
	router.HandleFunc("/health", GetHeath).Methods("GET")
	log.Fatal(http.ListenAndServe(":8000", router))
}

func main() {
	flag.Parse()
	lis, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", *port))
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
	var opts []grpc.ServerOption
	if *tls {
		if *certFile == "" {
			*certFile = testdata.Path("server1.pem")
		}
		if *keyFile == "" {
			*keyFile = testdata.Path("server1.key")
		}
		creds, err := credentials.NewServerTLSFromFile(*certFile, *keyFile)
		if err != nil {
			log.Fatalf("Failed to generate credentials %v", err)
		}
		opts = []grpc.ServerOption{grpc.Creds(creds)}
	}
	grpcServer := grpc.NewServer(opts...)
	s := newServer()
	pb.RegisterVeriServiceServer(grpcServer, s)
	go s.check()
	go s.restApi()
	log.Println("Server started .")
	grpcServer.Serve(lis)
}
