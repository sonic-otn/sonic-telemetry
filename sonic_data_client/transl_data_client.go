//  Package client provides a generic access layer for data available in system
package client

import (
	spb "github.com/Azure/sonic-telemetry/proto"
	transutil "github.com/Azure/sonic-telemetry/transl_utils"
	mapset "github.com/deckarep/golang-set"
	"github.com/golang/glog"
	"github.com/golang/protobuf/proto"
	gnmipb "github.com/openconfig/gnmi/proto/gnmi"
	gnmi_extpb "github.com/openconfig/gnmi/proto/gnmi_ext"
	"github.com/Workiva/go-datastructures/queue"
	"github.com/openconfig/ygot/ygot"
	"strconv"
	"sync"
	"time"
	"fmt"
	"reflect"
	"github.com/Azure/sonic-mgmt-common/translib"
	"github.com/Azure/sonic-telemetry/common_utils"
	"bytes"
	"encoding/json"
	"context"
)

const (
	DELETE  int = 0
	REPLACE int = 1
	UPDATE  int = 2
)

type TranslClient struct {
	prefix *gnmipb.Path
	/* GNMI Path to REST URL Mapping */
	path2URI map[*gnmipb.Path]string
	channel  chan struct{}
	q        *queue.PriorityQueue

	synced sync.WaitGroup  // Control when to send gNMI sync_response
	w      *sync.WaitGroup // wait for all sub go routines to finish
	mu     sync.RWMutex    // Mutex for data protection among routines for transl_client
	ctx context.Context //Contains Auth info and request info
	extensions []*gnmi_extpb.Extension
}

func NewTranslClient(prefix *gnmipb.Path, getpaths []*gnmipb.Path, ctx context.Context, extensions []*gnmi_extpb.Extension) (Client, error) {
	var client TranslClient
	var err error
	client.ctx = ctx
	client.prefix = prefix
	client.extensions = extensions
	if getpaths != nil {
		client.path2URI = make(map[*gnmipb.Path]string)
		/* Populate GNMI path to REST URL map. */
		err = transutil.PopulateClientPaths(prefix, getpaths, &client.path2URI)
	}

	if err != nil {
		return nil, err
	} else {
		return &client, nil
	}
}

func (c *TranslClient) Get(w *sync.WaitGroup) ([]*spb.Value, error) {
	rc, ctx := common_utils.GetContext(c.ctx)
	c.ctx = ctx
	var values []*spb.Value
	ts := time.Now()

	version := getBundleVersion(c.extensions)
	if version != nil {
		rc.BundleVersion = version
	}
	/* Iterate through all GNMI paths. */
	for gnmiPath, URIPath := range c.path2URI {
		/* Fill values for each GNMI path. */
		val, err := transutil.TranslProcessGet(URIPath, nil, c.ctx)

		if err != nil {
			return nil, err
		}

		/* Value of each path is added to spb value structure. */
		values = append(values, &spb.Value{
			Prefix:    c.prefix,
			Path:      gnmiPath,
			Timestamp: ts.UnixNano(),
			Val:       val,
		})
	}

	/* The values structure at the end is returned and then updates in notitications as
	specified in the proto file in the server.go */

	glog.V(2).Infof("TranslClient : Getting #%v", values)
	glog.V(1).Infof("TranslClient :Get done, total time taken: %v ms", int64(time.Since(ts)/time.Millisecond))

	return values, nil
}

func (c *TranslClient) Set(delete []*gnmipb.Path, replace []*gnmipb.Update, update []*gnmipb.Update) error {
	rc, ctx := common_utils.GetContext(c.ctx)
	c.ctx = ctx
	var uri string
	version := getBundleVersion(c.extensions)
	if version != nil {
		rc.BundleVersion = version
	}

	if (len(delete) + len(replace) + len(update)) > 1 {
		return transutil.TranslProcessBulk(delete, replace, update, c.prefix, c.ctx)
	} else {
		if len(delete) == 1 {
			/* Convert the GNMI Path to URI. */
			transutil.ConvertToURI(c.prefix, delete[0], &uri)
			return transutil.TranslProcessDelete(uri, c.ctx)
		}
		if len(replace) == 1 {
			/* Convert the GNMI Path to URI. */
			transutil.ConvertToURI(c.prefix, replace[0].GetPath(), &uri)
			return transutil.TranslProcessReplace(uri, replace[0].GetVal(), c.ctx)
		}
		if len(update) == 1 {
			/* Convert the GNMI Path to URI. */
			transutil.ConvertToURI(c.prefix, update[0].GetPath(), &uri)
			return transutil.TranslProcessUpdate(uri, update[0].GetVal(), c.ctx)
		}
	}
	return nil
}
func enqueFatalMsgTranslib(c *TranslClient, msg string) {
	c.q.Put(Value{
		&spb.Value{
			Timestamp: time.Now().UnixNano(),
			Fatal:     msg,
		},
	})
}
type ticker_info struct{
	t              *time.Ticker
	sub            *gnmipb.Subscription
	heartbeat      bool
}

func UpdatePrefixOriginByHostname(prefix *gnmipb.Path) {
	hostname := translib.GetSystemHostname()
	prefix.Origin = hostname
}

func (c *TranslClient) StreamRun(q *queue.PriorityQueue, stop chan struct{}, w *sync.WaitGroup, subscribe *gnmipb.SubscriptionList) {
	rc, ctx := common_utils.GetContext(c.ctx)
	c.ctx = ctx
	c.w = w

	defer c.w.Done()
	c.q = q
	c.channel = stop
	version := getBundleVersion(c.extensions)
	if version != nil {
		rc.BundleVersion = version
	}

	ticker_map := make(map[int][]*ticker_info)
	var cases []reflect.SelectCase
	cases_map := make(map[int]int)
	var subscribe_mode gnmipb.SubscriptionMode
	stringPaths := make([]string, len(subscribe.Subscription))
	for i,sub := range subscribe.Subscription {
		stringPaths[i] = c.path2URI[sub.Path]
	}
	req := translib.IsSubscribeRequest{Paths:stringPaths}
	subSupport,_ := translib.IsSubscribeSupported(req)
	var onChangeSubsString []string
	var onChangeSubsgNMI []*gnmipb.Path
	onChangeMap := make(map[string]*gnmipb.Path)
	valueCache := make(map[string]string)

	for i,sub := range subscribe.Subscription {
		glog.V(1).Info(sub.Mode, sub.SampleInterval)
		switch sub.Mode {

		case gnmipb.SubscriptionMode_TARGET_DEFINED:

			if subSupport[i].Err == nil && subSupport[i].IsOnChangeSupported {
				if subSupport[i].PreferredType == translib.Sample {
					subscribe_mode = gnmipb.SubscriptionMode_SAMPLE
				} else if subSupport[i].PreferredType == translib.OnChange {
					subscribe_mode = gnmipb.SubscriptionMode_ON_CHANGE
				}
			} else {
				subscribe_mode = gnmipb.SubscriptionMode_SAMPLE
			}

		case gnmipb.SubscriptionMode_ON_CHANGE:
			if subSupport[i].Err == nil && subSupport[i].IsOnChangeSupported {
				if (subSupport[i].MinInterval > 0) {
					subscribe_mode = gnmipb.SubscriptionMode_ON_CHANGE
				}else{
					enqueFatalMsgTranslib(c, fmt.Sprintf("Invalid subscribe path %v", stringPaths[i]))
					return
				}
			} else {
				enqueFatalMsgTranslib(c, fmt.Sprintf("ON_CHANGE Streaming mode invalid for %v", stringPaths[i]))
				return
			}
		case gnmipb.SubscriptionMode_SAMPLE:
			if (subSupport[i].MinInterval > 0) {
				subscribe_mode = gnmipb.SubscriptionMode_SAMPLE
			}else{
				enqueFatalMsgTranslib(c, fmt.Sprintf("Invalid subscribe path %v", stringPaths[i]))
				return
			}
		default:
			glog.Errorf("Bad Subscription Mode for client %s ", c)
			enqueFatalMsgTranslib(c, fmt.Sprintf("Invalid Subscription Mode %d", sub.Mode))
			return
		}
		glog.V(1).Info("subscribe_mode:", subscribe_mode)
		if subscribe_mode == gnmipb.SubscriptionMode_SAMPLE {
			interval := int(sub.SampleInterval)
			if interval == 0 {
				interval = subSupport[i].MinInterval * int(time.Second)
			} else {
				if interval < (subSupport[i].MinInterval*int(time.Second)) {
					enqueFatalMsgTranslib(c, fmt.Sprintf("Invalid Sample Interval %ds, minimum interval is %ds", interval/int(time.Second), subSupport[i].MinInterval))
					return
				}
			}

			addTimer(c, ticker_map, &cases, cases_map, interval, sub, false)

			//Heartbeat intervals are valid for SAMPLE in the case suppress_redundant is specified
			if sub.SuppressRedundant && sub.HeartbeatInterval > 0 {
				if int(sub.HeartbeatInterval) < subSupport[i].MinInterval * int(time.Second) {
					enqueFatalMsgTranslib(c, fmt.Sprintf("Invalid Heartbeat Interval %ds, minimum interval is %ds", int(sub.HeartbeatInterval)/int(time.Second), subSupport[i].MinInterval))
					return
				}
				addTimer(c, ticker_map, &cases, cases_map, int(sub.HeartbeatInterval), sub, true)
			}

			if !subscribe.UpdatesOnly {
				//Send initial data now so we can send sync response.
				val, err := transutil.TranslProcessGetRegex(c.path2URI[sub.Path], c.ctx)
				if err != nil {
					// process the rest of subscriptions while one query failed
					glog.Errorf("get regex for %s failed as %v", c.path2URI[sub.Path], err)
					continue
				}

				if len(val) == 0 {
					continue
				}

				UpdatePrefixOriginByHostname(c.prefix)
				var regexValues RegexValues
				regexValues.values = mapset.NewSet()
				for _, iter := range val {
					path, err := ygot.StringToStructuredPath(iter.Path)
					if err != nil {
						glog.Errorf("generate gnmipb.Path from path %s failed", iter.Path)
						return
					}
					spbv := &spb.Value{
						Prefix:       c.prefix,
						Path:         path,
						Timestamp:    time.Now().UnixNano(),
						SyncResponse: false,
						Val:          iter.TypedValue,
					}
					regexValues.values.Add(spbv)
					valueCache[c.path2URI[sub.Path]] = string(iter.TypedValue.GetJsonIetfVal())
				}
				c.q.Put(regexValues)
			}
		} else if subscribe_mode == gnmipb.SubscriptionMode_ON_CHANGE {
			onChangeSubsString = append(onChangeSubsString, c.path2URI[sub.Path])
			onChangeSubsgNMI = append(onChangeSubsgNMI, sub.Path)
			onChangeMap[c.path2URI[sub.Path]] = sub.Path
			if sub.HeartbeatInterval > 0 {
				if int(sub.HeartbeatInterval) < subSupport[i].MinInterval * int(time.Second) {
					enqueFatalMsgTranslib(c, fmt.Sprintf("Invalid Heartbeat Interval %ds, minimum interval is %ds", int(sub.HeartbeatInterval)/int(time.Second), subSupport[i].MinInterval))
					return
				}
				addTimer(c, ticker_map, &cases, cases_map, int(sub.HeartbeatInterval), sub, true)
			}
			
		}
	}
	if len(onChangeSubsString) > 0 {
		c.w.Add(1)
		c.synced.Add(1)
		go TranslSubscribe(onChangeSubsgNMI, onChangeSubsString, onChangeMap, c, subscribe.UpdatesOnly)

	}
	// Wait until all data values corresponding to the path(s) specified
	// in the SubscriptionList has been transmitted at least once
	c.synced.Wait()
	spbs := &spb.Value{
		Timestamp:    time.Now().UnixNano(),
		SyncResponse: true,
	}
	c.q.Put(Value{spbs})
	cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(c.channel)})

	for {
		chosen, _, ok := reflect.Select(cases)
		if !ok {
			return
		}

		UpdatePrefixOriginByHostname(c.prefix)
		for _,tick := range ticker_map[cases_map[chosen]] {
			glog.Infof("!!!tick, heartbeat: %t, path: %s\n", tick.heartbeat, c.path2URI[tick.sub.Path])
			val, err := transutil.TranslProcessGetRegex(c.path2URI[tick.sub.Path], c.ctx)
			if err != nil {
				glog.Errorf("get regex for %s failed as %v", c.path2URI[tick.sub.Path], err)
				continue
			}

			if len(val) == 0 {
				continue
			}

			var regexValues RegexValues
			regexValues.values = mapset.NewSet()
			for _, iter := range val {
				path, err := ygot.StringToStructuredPath(iter.Path)
				if err != nil {
					glog.Errorf("generate gnmipb.Path from path %s failed", iter.Path)
					continue
				}
				spbv := &spb.Value{
					Prefix:       c.prefix,
					Path:         path,
					Timestamp:    time.Now().UnixNano(),
					SyncResponse: false,
					Val:          iter.TypedValue,
				}

				if (tick.sub.SuppressRedundant) && (!tick.heartbeat) && (string(iter.TypedValue.GetJsonIetfVal()) == valueCache[c.path2URI[tick.sub.Path]]) {
					glog.V(1).Infof("Redundant Message Suppressed #%v", string(iter.TypedValue.GetJsonIetfVal()))
					continue
				}

				valueCache[c.path2URI[tick.sub.Path]] = string(iter.TypedValue.GetJsonIetfVal())
				glog.V(2).Infof("Added spbv #%v", spbv)
				regexValues.values.Add(spbv)
			}
			c.q.Put(regexValues)
		}
	}
}

func addTimer(c *TranslClient, ticker_map map[int][]*ticker_info, cases *[]reflect.SelectCase, cases_map map[int]int, interval int, sub *gnmipb.Subscription, heartbeat bool) {
	//Reuse ticker for same sample intervals, otherwise create a new one.
	if ticker_map[interval] == nil {
		ticker_map[interval] = make([]*ticker_info, 1, 1)
		ticker_map[interval][0] = &ticker_info {
			t: time.NewTicker(time.Duration(interval) * time.Nanosecond),
			sub: sub,
			heartbeat: heartbeat,
		}
		cases_map[len(*cases)] = interval
		*cases = append(*cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ticker_map[interval][0].t.C)})
	}else {
		ticker_map[interval] = append(ticker_map[interval], &ticker_info {
			t: ticker_map[interval][0].t,
			sub: sub,
			heartbeat: heartbeat,
		})
	}
	

}

func genGnmiPathKeyByPayload(payload []byte) map[string]string {
	gnmiPathKey := make(map[string]string)
	m := make(map[string]interface{})
	err := json.Unmarshal(payload, &m)
	if err != nil {
		glog.Errorf("convert payload to map failed as %v", err)
		return nil
	}

	for k, v := range m {
		switch v.(type) {
		case string:
			val := v.(string)
			gnmiPathKey[k] = val
		case float64:
			val := v.(float64)
			strVal := strconv.FormatFloat(val, 'f', -1, 64)
			gnmiPathKey[k] = strVal
		default:
			glog.Infof("convert payload field %s into gnmiPath key failed", k)
		}
	}

	return gnmiPathKey
}

func constructSpbValueBySubRsp(subRsp *translib.SubscribeResponse) *spb.Value {
	var prefix gnmipb.Path

	UpdatePrefixOriginByHostname(&prefix)

	path, err := ygot.StringToStructuredPath(subRsp.Path)
	if err != nil {
		glog.Errorf("generate gnmipb.Path from path %s failed", subRsp.Path)
		return nil
	}

	var val *gnmipb.TypedValue
	if subRsp.Payload != nil {
		dst := new(bytes.Buffer)
		json.Compact(dst, subRsp.Payload)
		jv := dst.Bytes()

		if subRsp.IsDeleted {
			lastElem := path.Elem[len(path.Elem) - 1]
			lastElem.Key = genGnmiPathKeyByPayload(jv)
		} else {
			/* Fill the values into GNMI data structures . */
			val = &gnmipb.TypedValue{
				Value: &gnmipb.TypedValue_JsonIetfVal{
					JsonIetfVal: jv,
				}}
		}
	}

	spbv := &spb.Value{
		Prefix:       &prefix,
		Path:         path,
		Timestamp:    subRsp.Timestamp,
		SyncResponse: false,
		Val:          val,
	}

	return spbv
}

func TranslSubscribe(gnmiPaths []*gnmipb.Path, stringPaths []string, pathMap map[string]*gnmipb.Path, c *TranslClient, updates_only bool) {
	defer c.w.Done()
	rc, ctx := common_utils.GetContext(c.ctx)
	c.ctx = ctx
	q := queue.NewPriorityQueue(1, false)
	var sync_done bool
	req := translib.SubscribeRequest{Paths:stringPaths, Q:q, Stop:c.channel}
	if rc.BundleVersion != nil {
		nver, err := translib.NewVersion(*rc.BundleVersion)
		if err != nil {
			glog.Errorf("Subscribe operation failed with error =%v", err.Error())
			enqueFatalMsgTranslib(c, fmt.Sprintf("Subscribe operation failed with error =%v", err.Error()))
			return
		}
		req.ClientVersion = nver
	}
	_, err := translib.Subscribe(req)
	if err != nil {
		glog.Errorf("translib subscribe failed %v", err)
		return
	}
	for {
		items, err := q.Get(1)
		if err != nil {
			glog.Errorf("%v", err)
			return
		}
		switch v := items[0].(type) {
		case *translib.SubscribeResponse:

			if v.IsTerminated {
				//DB Connection or other backend error
				enqueFatalMsgTranslib(c, "DB Connection Error")
				close(c.channel)
				return
			}

			spbv := constructSpbValueBySubRsp(v)
			if spbv == nil {
				glog.Errorf("generate gnmipb.Value for %s failed", v.Path)
				continue
			}

			//Don't send initial update with full object if user wants updates only.
			if updates_only && !sync_done {
				glog.V(1).Infof("Msg suppressed due to updates_only")
			} else {
				c.q.Put(Value{spbv})
			}

			glog.V(2).Infof("Added spbv #%v", spbv)
			
			if v.SyncComplete && !sync_done {
				glog.V(1).Info("SENDING SYNC")
				c.synced.Done()
				sync_done = true
			}
		default:
			glog.V(1).Infof("Unknown data type %v for %s in queue", items[0], c)
		}
	}
}



func (c *TranslClient) PollRun(q *queue.PriorityQueue, poll chan struct{}, w *sync.WaitGroup, subscribe *gnmipb.SubscriptionList) {
	rc, ctx := common_utils.GetContext(c.ctx)
	c.ctx = ctx
	c.w = w
	defer c.w.Done()
	c.q = q
	c.channel = poll
	version := getBundleVersion(c.extensions)
	if version != nil {
		rc.BundleVersion = version
	}
	synced := false
	for {
		_, more := <-c.channel
		if !more {
			glog.V(1).Infof("%v poll channel closed, exiting pollDb routine", c)
			return
		}
		t1 := time.Now()
		for gnmiPath, URIPath := range c.path2URI {
			if synced || !subscribe.UpdatesOnly {
				val, err := transutil.TranslProcessGet(URIPath, nil, c.ctx)
				if err != nil {
					return
				}

				spbv := &spb.Value{
					Prefix:       c.prefix,
					Path:         gnmiPath,
					Timestamp:    time.Now().UnixNano(),
					SyncResponse: false,
					Val:          val,
				}

				c.q.Put(Value{spbv})
				glog.V(2).Infof("Added spbv #%v", spbv)
			}
		}

		c.q.Put(Value{
			&spb.Value{
				Timestamp:    time.Now().UnixNano(),
				SyncResponse: true,
			},
		})
		synced = true
		glog.V(1).Infof("Sync done, poll time taken: %v ms", int64(time.Since(t1)/time.Millisecond))
	}
}
func (c *TranslClient) OnceRun(q *queue.PriorityQueue, once chan struct{}, w *sync.WaitGroup, subscribe *gnmipb.SubscriptionList) {
	rc, ctx := common_utils.GetContext(c.ctx)
	c.ctx = ctx
	c.w = w
	defer c.w.Done()
	c.q = q
	c.channel = once

	version := getBundleVersion(c.extensions)
	if version != nil {
		rc.BundleVersion = version
	}
	_, more := <-c.channel
	if !more {
		glog.V(1).Infof("%v once channel closed, exiting onceDb routine", c)
		return
	}
	t1 := time.Now()
	for gnmiPath, URIPath := range c.path2URI {
		val, err := transutil.TranslProcessGet(URIPath, nil, c.ctx)
		if err != nil {
			return
		}

		if !subscribe.UpdatesOnly && val != nil {
			spbv := &spb.Value{
				Prefix:       c.prefix,
				Path:         gnmiPath,
				Timestamp:    time.Now().UnixNano(),
				SyncResponse: false,
				Val:          val,
			}

			c.q.Put(Value{spbv})
			glog.V(2).Infof("Added spbv #%v", spbv)
		}
	}

	c.q.Put(Value{
		&spb.Value{
			Timestamp:    time.Now().UnixNano(),
			SyncResponse: true,
		},
	})
	glog.V(1).Infof("Sync done, once time taken: %v ms", int64(time.Since(t1)/time.Millisecond))
	
}

func (c *TranslClient) Capabilities() []gnmipb.ModelData {
	rc, ctx := common_utils.GetContext(c.ctx)
	c.ctx = ctx
	version := getBundleVersion(c.extensions)
	if version != nil {
		rc.BundleVersion = version
	}
	/* Fetch the supported models. */
	supportedModels := transutil.GetModels()
	return supportedModels
}

func (c *TranslClient) Close() error {
	return nil
}

func getBundleVersion(extensions []*gnmi_extpb.Extension) *string {
	for _,e := range extensions {
		switch v := e.Ext.(type) {
			case *gnmi_extpb.Extension_RegisteredExt:
				if v.RegisteredExt.Id == spb.BUNDLE_VERSION_EXT {
					var bv spb.BundleVersion
					proto.Unmarshal(v.RegisteredExt.Msg, &bv)
					return &bv.Version
				}
			
		}
	}
	return nil
}
