package telemetry_dialout

import (
	// "encoding/json"
	"crypto/tls"
	"errors"
	"fmt"
	"github.com/Azure/sonic-telemetry/common_utils"
	spb "github.com/Azure/sonic-telemetry/proto"
	sdc "github.com/Azure/sonic-telemetry/sonic_data_client"
	sdcfg "github.com/Azure/sonic-telemetry/sonic_db_config"
	"github.com/Workiva/go-datastructures/queue"
	"github.com/go-redis/redis"
	"github.com/golang/glog"
	gpb "github.com/openconfig/gnmi/proto/gnmi"
	"github.com/openconfig/ygot/ygot"
	"golang.org/x/net/context"
	"google.golang.org/grpc"

	// "google.golang.org/grpc/credentials"
	"net"
	//"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// Unknown is an unknown report and should always be treated as an error.
	Unknown reportType = iota
	// Once will perform a Once report against the agent.
	Once
	// Poll will perform a Periodic report against the agent.
	Periodic
	// Stream will perform a Streaming report against the agent.
	Stream
)

// Type defines the type of report.
type reportType int

// NewType returns a new reportType based on the provided string.
func NewReportType(s string) reportType {
	v, ok := typeConst[s]
	if !ok {
		return Unknown
	}
	return v
}

// String returns the string representation of the reportType.
func (r reportType) String() string {
	return typeString[r]
}

var (
	typeString = map[reportType]string{
		Unknown:  "unknown",
		Once:     "once",
		Periodic: "periodic",
		Stream:   "stream",
	}

	typeConst = map[string]reportType{
		"unknown":  Unknown,
		"once":     Once,
		"periodic": Periodic,
		"stream":   Stream,
	}
	clientCfg *ClientConfig
	// Global mutex for protecting the config data
	configMu sync.Mutex

	// Each Destination group may have more than one Destinations
	// Only one destination will be used at one time
	destGrpNameMap = make(map[string][]Destination)

	// For finding clientSubscription quickly
	ClientSubscriptionNameMap = make(map[string]*clientSubscription)

	// map for storing name of clientSubscription which are users of the destination group
	DestGrp2ClientSubMap = make(map[string][]string)
)

type Destination struct {
	Addrs string
}

func (d Destination) Validate() error {
	if len(d.Addrs) == 0 {
		return errors.New("Destination.Addrs is empty")
	}
	// TODO: validate Addrs is in format IP:PORT
	return nil
}

// Global config for all clients
type ClientConfig struct {
	SrcIp          string
	RetryInterval  time.Duration
	Encoding       gpb.Encoding
	Unidirectional bool        // by default, no reponse from remote server
	TLS            *tls.Config // TLS config to use when connecting to target. Optional.
	RedisConType   string      // "unix"  or "tcp"
}

// clientSubscription is the container for config data,
// it also keeps mapping from destination to running publish Client instance
type clientSubscription struct {
	// Config Data
	name          string
	destGroupName string
	prefix        *gpb.Path
	paths         []*gpb.Path
	reportType    reportType
	interval      time.Duration // report interval
	heartbeatInterval    time.Duration // heartbeat interval

	// Running time data
	cMu    sync.Mutex
	client *Client              // GNMIDialOutClient
	dc     sdc.Client           // SONiC data client
	stop   chan struct{}        // Inform publishRun routine to stop
	q      *queue.PriorityQueue // for data passing among go routine
	w      sync.WaitGroup       // Wait for all sub go routine to finish
	opened bool                 // whether there is opened instance for this client subscription
	cancel context.CancelFunc

	conTryCnt uint64 //Number of time trying to connect
	sendMsg   uint64
	recvMsg   uint64
	errors    uint64
}

// Client handles execution of the telemetry publish service.
type Client struct {
	conn *grpc.ClientConn

	mu      sync.Mutex
	client  spb.GNMIDialOutClient
	publish spb.GNMIDialOut_PublishClient

	// dataChan chan struct{} //to pass data struct pointer
	//
	// synced  sync.WaitGroup
	sendMsg uint64
	recvMsg uint64
}

func (cs *clientSubscription) Close() {
	cs.cMu.Lock()
	defer cs.cMu.Unlock()
	if cs.opened == false {
		glog.Infof("Opened is false: %v", cs.name)
		return
	}
	if cs.stop != nil {
		close(cs.stop) //Inform the clientSubscription publish service routine to stop
	}

	if cs.q != nil {
		if !cs.q.Disposed() {
			cs.q.Dispose()
		}
	}
	if cs.client != nil {

		cs.client.Close() // Close GNMIDialOutClient
	}
	cs.opened = false
	glog.Infof("Closed %v", cs.name)
}

func (cs *clientSubscription) NewInstance(ctx context.Context) error {
	cs.cMu.Lock()
	defer cs.cMu.Unlock()

	if cs.destGroupName == "" {
		glog.Errorf("Destination group is not set for %v", cs)
		return fmt.Errorf("Destination group is not set for %v", cs)
	}

	dests, ok := destGrpNameMap[cs.destGroupName]
	if !ok {
		glog.Errorf("Destination group %v doesn't exist", cs.destGroupName)
		return fmt.Errorf("Destination group %v doesn't exist", cs.destGroupName)
	}

	target := cs.prefix.GetTarget()
	if target == "" {
		return fmt.Errorf("Empty target data not supported yet")
	}

	// Connection to system data source
	var dc sdc.Client
	var err error
	if target == "OTHERS" {
		dc, err = sdc.NewNonDbClient(cs.paths, cs.prefix)
	} else if common_utils.IsTargetDb(target) == true {
		dc, err = sdc.NewDbClient(cs.paths, cs.prefix)
	} else {
		/* For any other target or no target create new Transl Client. */
		dc, err = sdc.NewTranslClient(cs.prefix, cs.paths, ctx, nil)
	}
	if err != nil {
		glog.Errorf("Connection to DB for %v failed: %v", *cs, err)
		return fmt.Errorf("Connection to DB for %v failed: %v", *cs, err)
	}
	cs.dc = dc
	go publishRun(ctx, cs, dests)
	glog.V(1).Infof("publishRun for %v with destination %v", cs, dests)
	return nil
}

// send runs until process Queue returns an error.
func (cs *clientSubscription) send(stream spb.GNMIDialOut_PublishClient) error {
	for {
		items, err := cs.q.Get(1)

		if items == nil {
			glog.Errorf("send error as %v", err)
			return err
		}
		if err != nil {
			cs.errors++
			glog.Errorf("send error as %v", err)
			return fmt.Errorf("unexpected queue Gext(1): %v", err)
		}

		var resp *gpb.SubscribeResponse
		switch v := items[0].(type) {
		case sdc.RegexValues:
			if resp, err = sdc.ValuesToResp(v); err != nil {
				cs.errors++
				return err
			}
		case sdc.Value:
			if resp, err = sdc.ValToResp(v); err != nil {
				cs.errors++
				return err
			}
		default:
			glog.Errorf("Unknown data type %v for %s in queue", items[0], cs)
			cs.errors++
		}

		cs.sendMsg++
		err = stream.Send(resp)
		if err != nil {
			glog.Errorf("Client %s sending error:%v", cs, err)
			cs.errors++
			return err
		}
		glog.V(1).Infof("Client %s done sending, msg count %d, msg %v", cs, cs.sendMsg, resp)
	}
}

// String returns the target the client is querying.
func (cs *clientSubscription) String() string {
	return fmt.Sprintf(" %s:%s:%s prefix %v paths %v interval %v, sendMsg %v, recvMsg %v",
		cs.name, cs.destGroupName, cs.reportType, cs.prefix.GetTarget(), cs.paths, cs.interval, cs.sendMsg, cs.recvMsg)
}

// newClient returns a new initialized GNMIDialout client.
// it connects to destination and publish service
// TODO: TLS credential support
func newClient(ctx context.Context, dest Destination) (*Client, error) {
	timeout := clientCfg.RetryInterval
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	opts := []grpc.DialOption{
		grpc.WithBlock(),
		//grpc.WithInsecure(),
	}
	if clientCfg.TLS != nil {
		//opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(clientCfg.TLS)))
	}
	conn, err := grpc.DialContext(ctx, dest.Addrs, opts...)
	if err != nil {
		return nil, fmt.Errorf("Dial to (%s, timeout %v): %v", dest, timeout, err)
	}
	cl := spb.NewGNMIDialOutClient(conn)
	return &Client{
		conn:   conn,
		client: cl,
	}, nil
}

// Closing of client queue is triggered upon end of stream receive or stream error
// or fatal error of any client go routine .
// it will cause cancle of client context and exit of the send goroutines.
func (c *Client) Close() error {
	return c.conn.Close()
}

func publishRun(ctx context.Context, cs *clientSubscription, dests []Destination) {
	var err error
	var c *Client
	var destNum, destIdx int
	destNum = len(dests)
	destIdx = 0

restart: //Remote server might go down, in that case we restart with next destination in the group
	cs.cMu.Lock()
	cs.stop = make(chan struct{}, 1)
	cs.q = queue.NewPriorityQueue(1, false)
	cs.opened = true
	cs.client = nil
	cs.cMu.Unlock()

	cs.conTryCnt++
	dest := dests[destIdx]
	destIdx = (destIdx + 1) % destNum
	c, err = newClient(ctx, dest)
	select {
	case <-ctx.Done():
		cs.Close()
		glog.Errorf("%v: %v, cs.conTryCnt %v", cs.name, err, cs.conTryCnt)
		return
	default:
	}
	if err != nil {
		glog.Errorf("Dialout connection for %v failed for %v, %v cs.conTryCnt %v", dest, cs.name, err, cs.conTryCnt)
		goto restart
	}

	glog.Infof("Dialout service connected to %v successfully for %v", dest, cs.name)
	pub, err := c.client.Publish(ctx)
	if err != nil {
		glog.Errorf("Publish to %v for %v failed: %v, retrying", dest, cs.name, err)
		c.Close()
		cs.Close()
		goto restart
	}

	cs.cMu.Lock()
	if cs.client == nil {
		cs.client = c
	} else {
		glog.Errorf("connection to %v already exists for %v, exiting publishRun", dest, cs)
		c.Close()
		cs.cMu.Unlock()
		return
	}
	cs.cMu.Unlock()

	switch cs.reportType {
	case Periodic:
		for {
			select {
			default:
				spbValues, err := cs.dc.Get(nil)
				if err != nil {
					// TODO: need to inform
					glog.Errorf("Data read error %v for %v", err, cs)
					continue
					//return nil, status.Error(codes.NotFound, err.Error())
				}
				var updates []*gpb.Update
				var spbValue *spb.Value
				for _, spbValue = range spbValues {
					update := &gpb.Update{
						Path: spbValue.GetPath(),
						Val:  spbValue.GetVal(),
					}
					updates = append(updates, update)
				}
				rs := &gpb.SubscribeResponse_Update{
					Update: &gpb.Notification{
						Timestamp: spbValue.GetTimestamp(),
						Prefix:    cs.prefix,
						Update:    updates,
					},
				}
				response := &gpb.SubscribeResponse{Response: rs}

				glog.V(2).Infof("cs %s sending \n\t%v \n To %s", cs.name, response, dest)
				err = pub.Send(response)
				if err != nil {
					glog.Errorf("Client %v pub Send error:%v, cs.conTryCnt %v", cs.name, err, cs.conTryCnt)
					cs.Close()
					// Retry
					goto restart
				}
				glog.V(1).Infof("cs %s to  %s done", cs.name, dest)
				cs.sendMsg++
				c.sendMsg++

				time.Sleep(cs.interval)
			case <-cs.stop:
				glog.Infof("%v exiting publishRun routine for destination %s", cs, dest)
				return
			}
		}
	case Stream:
		select {
		default:
			cs.w.Add(1)

			s := prepareSubscriptionList(cs)
			go cs.dc.StreamRun(cs.q, cs.stop, &cs.w, s)
			time.Sleep(100 * time.Millisecond)
			err = cs.send(pub)
			if err != nil {
				glog.Errorf("Client %v pub Send error:%v, cs.conTryCnt %v", cs.name, err, cs.conTryCnt)
			}
			cs.Close()
			cs.w.Wait()
			glog.V(1).Infof("%s closes sub goroutines success and restart begin", cs.name)
			// Don't restart immediatly
			time.Sleep(time.Second * 2)
			goto restart

		case <-cs.stop:
			glog.Infof("%v exiting publishRun routine for destination %s", cs.name, dest)
			return
		}
	default:
		glog.Errorf("Unsupported report type %s in %v ", cs.reportType, cs)
	}
}

func prepareSubscriptionList(cs *clientSubscription) *gpb.SubscriptionList {
	target := cs.prefix.GetTarget()
	if target != "OC-YANG" {
		return nil
	}

	s := &gpb.SubscriptionList{
		Mode: gpb.SubscriptionList_STREAM,
		Prefix: cs.prefix,
	}

	for _, path := range cs.paths {
		mode := gpb.SubscriptionMode_SAMPLE
		if cs.interval == 0 {
			mode = gpb.SubscriptionMode_ON_CHANGE
		}
		s.Subscription = append(s.Subscription, &gpb.Subscription{
			Path: path,
			Mode: mode,
			SampleInterval: uint64(cs.interval),
			HeartbeatInterval: uint64(cs.heartbeatInterval),
			SuppressRedundant: false,
		})
	}
	return s
}

/*
	// telemetry client  global configuration
	Key         = TELEMETRY_CLIENT|Global
	src_ip      = IP
	retry_interval = 1*4DIGIT     ; In second
	encoding    = "JSON_IETF" / "ASCII" / "BYTES" / "PROTO"
	unidirectional = "true" / "false"    ; true by default

	// Destination group
	Key      = TELEMETRY_CLIENT|DestinationGroup_<name>
	dst_addr   = IP1:PORT2,IP2:PORT2       ;IP addresses separated by ","

	PORT = 1*5DIGIT
	IP = dec-octet "." dec-octet "." dec-octet "." dec-octet

	// Subscription group
	Key         = TELEMETRY_CLIENT|Subscription_<name>
	path_target = DbName
	paths       = PATH1,PATH2        ;PATH separated by ","
	dst_group   = <name>      ; // name of DestinationGroup
	report_type = "periodic" / "stream" / "once"
	report_interval = 1*8DIGIT      ; In millisecond,
*/

// closeDestGroupClient close client instances for all clientSubscription using
// this Destination Group
func closeDestGroupClient(destGroupName string) {
	if names, ok := DestGrp2ClientSubMap[destGroupName]; ok {
		for _, name := range names {
			cs := ClientSubscriptionNameMap[name]
			cs.Close()
			cs.cancel()
		}
	}
}

// setupDestGroupClients create client instances for all clientSubscription using
// this Destination Group
func setupDestGroupClients(ctx context.Context, destGroupName string) {
	if names, ok := DestGrp2ClientSubMap[destGroupName]; ok {
		for _, name := range names {
			// Create a copy of Client subscription, existing one might be closing, don't interfere with it.
			cs := *ClientSubscriptionNameMap[name]
			glog.V(1).Infof("NewInstance with destGroup change for %s to %s", name, destGroupName)
			cs.NewInstance(ctx)
			ClientSubscriptionNameMap[name] = &cs
		}
	}
}

// start/stop/update telemetry publist client as requested
// TODO: more validation on db data
func processTelemetryClientConfig(ctx context.Context, redisDb *redis.Client, key string, op string) error {
	separator, _ := sdc.GetTableKeySeparator("CONFIG_DB")
	tableKey := "TELEMETRY_CLIENT" + separator + key
	fv, err := redisDb.HGetAll(tableKey).Result()
	if err != nil {
		glog.Errorf("redis HGetAll failed for %s with error %v", tableKey, err)
		return fmt.Errorf("redis HGetAll failed for %s with error %v", tableKey, err)
	}

	glog.V(1).Infof("Processing %v %v", tableKey, fv)
	configMu.Lock()
	defer configMu.Unlock()

	ctx, cancel := context.WithCancel(ctx)

	if key == "Global" {
		if op == "hdel" {
			glog.V(1).Infof("Invalid delete operation for %v", tableKey)
			return fmt.Errorf("Invalid delete operation for %v", tableKey)
		} else {
			for field, value := range fv {
				switch field {
				case "src_ip":
					clientCfg.SrcIp = value
				case "retry_interval":
					//TODO: check validity of the interval
					itvl, err := strconv.ParseUint(value, 10, 64)
					if err != nil {
						glog.Errorf("Invalid retry_interval %v %v", value, err)
						continue
					}
					clientCfg.RetryInterval = time.Second * time.Duration(itvl)
				case "encoding":
					//Flexible encoding Not supported yet
					clientCfg.Encoding = gpb.Encoding_JSON_IETF
				case "unidirectional":
					// No PublishResponse supported yet
					clientCfg.Unidirectional = true
				}
			}
			// Apply changes to all running instances
			//for grpName := range destGrpNameMap {
			//	closeDestGroupClient(grpName)
			//	setupDestGroupClients(ctx, grpName)
			//}
		}
	} else if strings.HasPrefix(key, "DestinationGroup_") {
		destGroupName := strings.TrimPrefix(key, "DestinationGroup_")
		if destGroupName == "" {
			return fmt.Errorf("Empty  Destination Group name %v", key)
		}
		// Close any client intances targeting this Destination group
		//closeDestGroupClient(destGroupName)
		//DestGrp2ClientSubMap
		if op == "hdel" {
			if _, ok := DestGrp2ClientSubMap[destGroupName]; ok {
				glog.Errorf("%v is being used: %v", destGroupName, DestGrp2ClientSubMap)
				return fmt.Errorf("%v is being used: %v", destGroupName, DestGrp2ClientSubMap)
			}
			delete(destGrpNameMap, destGroupName)
			glog.V(1).Infof("Deleted  DestinationGroup %v", destGroupName)
			return nil
		} else {
			var dests []Destination
			for field, value := range fv {
				switch field {
				case "dst_addr":
					addrs := strings.Split(value, ",")
					for _, addr := range addrs {
						dst := Destination{Addrs: addr}
						if err = dst.Validate(); err != nil {
							glog.Errorf("Invalid destination address %v", addrs)
							return fmt.Errorf("Invalid destination address %v", addrs)
						}
						dests = append(dests, Destination{Addrs: addr})
					}
				default:
					glog.Errorf("Invalid DestinationGroup value %v", value)
					return fmt.Errorf("Invalid DestinationGroup value %v", value)
				}
			}
			destGrpNameMap[destGroupName] = dests
			//setupDestGroupClients(ctx, destGroupName)
		}
	} else if strings.HasPrefix(key, "Subscription_") {
		name := strings.TrimPrefix(key, "Subscription_")
		if name == "" {
			return fmt.Errorf("Empty Subscription_ name %v", key)
		}
		csub, ok := ClientSubscriptionNameMap[name]
		if ok {
			csub.Close()
			csub.cancel()
		}

		if op == "hdel" {
			destGrpName := csub.destGroupName
			// Remove this ClientSubscrition from the list of the Destination group users
			csNames := DestGrp2ClientSubMap[destGrpName]
			for i, csName := range csNames {
				if name == csName {
					csNames = append(csNames[:i], csNames[i+1:]...)
					break
				}
			}
			DestGrp2ClientSubMap[destGrpName] = csNames
			// Delete clientSubscription from name map
			delete(ClientSubscriptionNameMap, name)
			glog.V(1).Infof("Deleted  Client Subscription %v", name)
			return nil
		} else {
			// TODO: start one subscription publish routine for this request
			// Only start routine when DestGrp2ClientSubMap is not empty, or ...?
			cs := clientSubscription{
				interval: 5000, // default to 5000 milliseconds
				name:     name,
				cancel:   cancel,
				prefix:   &gpb.Path{},
			}
			for field, value := range fv {
				switch field {
				case "dst_group":
					cs.destGroupName = value
				case "report_type":
					cs.reportType = NewReportType(value)
				case "report_interval":
					intvl, err := strconv.ParseUint(value, 10, 64)
					if err != nil {
						glog.Errorf("Invalid report_interval %v %v", value, err)
						continue
					}
					cs.interval = time.Duration(intvl) * time.Millisecond
				case "path_target":
					cs.prefix.Target = value
					sdc.UpdatePrefixOriginByHostname(cs.prefix)
				case "paths":
					ps := strings.Split(value, ",")
					newPaths := []*gpb.Path{}
					for _, p := range ps {
						pp, err := ygot.StringToPath(p, ygot.StructuredPath)
						if err != nil {
							glog.Errorf("Invalid paths %v", value)
							return fmt.Errorf("Invalid paths %v", value)
						}
						// append *gpb.Path
						newPaths = append(newPaths, pp)
					}
					cs.paths = newPaths
				case "heartbeat_interval":
					intvl, err := strconv.ParseUint(value, 10, 64)
					if err != nil {
						glog.Errorf("Invalid heartbeat_interval %v %v", value, err)
						continue
					}
					cs.heartbeatInterval = time.Duration(intvl) * time.Millisecond
				default:
					glog.Errorf("Invalid field %v value %v", field, value)
					return fmt.Errorf("Invalid field %v value %v", field, value)
				}
			}
			glog.V(1).Infof("New clientSubscription %v", cs)
			if cs.destGroupName == "" {
				// not destination configured, just return
				return nil
			}

			var found bool
			for _, na := range DestGrp2ClientSubMap[cs.destGroupName] {
				if na == cs.name {
					found = true
					break
				}
			}
			if !found {
				// Add this clientSubscription to the user list of Destination group
				DestGrp2ClientSubMap[cs.destGroupName] = append(DestGrp2ClientSubMap[cs.destGroupName], cs.name)
			}
			ClientSubscriptionNameMap[cs.name] = &cs
			glog.V(1).Infof("NewInstance with Subscription change for %s to %s", cs.name, cs.destGroupName)
			cs.NewInstance(ctx)
		}
	}
	return nil
}

func processExistedTelemetryClientData(ctx context.Context, redisDb *redis.Client) error {
	var err error
	var dbKeys []string

	separator, _ := sdc.GetTableKeySeparator("CONFIG_DB")
	keyPrefix := "TELEMETRY_CLIENT" + separator

	processDataByKeyword := func(keyword string) error {
		pattern := keyword + "*"
		dbKeys, err = redisDb.Keys(pattern).Result()
		if err != nil {
			glog.Errorf("get redis Keys by pattern [%v] failed with err %v", pattern, err)
			return err
		}
		for _, dbKey := range dbKeys {
			dbKey = dbKey[len(keyPrefix):]
			processTelemetryClientConfig(ctx, redisDb, dbKey, "hset")
		}

		return nil
	}

	err = processDataByKeyword(keyPrefix + "Global")
	if err != nil {
		return err
	}
	processDataByKeyword(keyPrefix + "DestinationGroup_")
	if err != nil {
		return err
	}
	processDataByKeyword(keyPrefix + "Subscription_")
	if err != nil {
		return err
	}

	return err
}

// read configDB data for telemetry client and start publishing service for client subscription
func DialOutRun(ctx context.Context, ccfg *ClientConfig) error {
	clientCfg = ccfg
	dbn := sdcfg.GetDbId("CONFIG_DB")

	var redisDb *redis.Client
	if sdc.UseRedisLocalTcpPort == false {
		redisDb = redis.NewClient(&redis.Options{
			Network:     "unix",
			Addr:        sdcfg.GetDbSock("CONFIG_DB"),
			Password:    "", // no password set
			DB:          dbn,
			DialTimeout: 0,
		})
	} else {
		redisDb = redis.NewClient(&redis.Options{
			Network:     "tcp",
			Addr:        sdcfg.GetDbTcpAddr("CONFIG_DB"),
			Password:    "", // no password set
			DB:          dbn,
			DialTimeout: 0,
		})
	}

	defer redisDb.Close()

	separator, _ := sdc.GetTableKeySeparator("CONFIG_DB")
	pattern := "__keyspace@" + strconv.Itoa(int(dbn)) + "__:TELEMETRY_CLIENT" + separator
	prefixLen := len(pattern)
	pattern += "*"

	pubsub := redisDb.PSubscribe(pattern)
	defer pubsub.Close()

	msgi, err := pubsub.ReceiveTimeout(time.Second)
	if err != nil {
		glog.Errorf("psubscribe to %s failed %v", pattern, err)
		return fmt.Errorf("psubscribe to %s failed %v", pattern, err)
	}
	subscr := msgi.(*redis.Subscription)
	if subscr.Channel != pattern {
		glog.Errorf("psubscribe to %s failed", pattern)
		return fmt.Errorf("psubscribe to %s", pattern)
	}
	glog.V(1).Infof("Psubscribe succeeded: %v", subscr)

	err = processExistedTelemetryClientData(ctx, redisDb)
	if err != nil {
		return err
	}

	for {
		msgi, err := pubsub.ReceiveTimeout(time.Millisecond * 1000)
		if err != nil {
			neterr, ok := err.(net.Error)
			if ok {
				if neterr.Timeout() == true {
					continue
				}
			}
			glog.Errorf("pubsub.ReceiveTimeout err %v", err)
			continue
		}
		subscr := msgi.(*redis.Message)
		dbkey := subscr.Channel[prefixLen:]
		if subscr.Payload == "del" || subscr.Payload == "hdel" {
			processTelemetryClientConfig(ctx, redisDb, dbkey, "hdel")
		} else if subscr.Payload == "hset" {
			processTelemetryClientConfig(ctx, redisDb, dbkey, "hset")
		} else {
			glog.Errorf("Invalid psubscribe payload notification:  %v", subscr)
			continue
		}
		// Check if ctx was canceled.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
}
