package elastic

import (
	"crypto/tls"
	"fmt"
	log "github.com/cihub/seelog"
	"infini.sh/framework/core/elastic"
	"infini.sh/framework/core/global"
	"infini.sh/framework/core/rate"
	"infini.sh/framework/core/stats"
	task2 "infini.sh/framework/core/task"
	"infini.sh/framework/core/util"
	"infini.sh/framework/lib/fasthttp"
	"infini.sh/gateway/proxy/balancer"
	"math/rand"
	"time"
)

type ReverseProxy struct {
	oldAddr     string
	bla         balancer.IBalancer
	clients     []*fasthttp.HostClient
	proxyConfig *ProxyConfig
	endpoints   []string
	lastNodesTopologyVersion int
}

func isEndpointValid(node elastic.NodesInfo, cfg *ProxyConfig) bool {

	log.Tracef("valid endpoint %v",node.Http.PublishAddress)
	var hasExclude =false
	var hasInclude =false
	endpoint:=node.Http.PublishAddress
	for _, v := range cfg.Filter.Hosts.Exclude {
		hasExclude=true
		if endpoint==v{
			log.Debugf("host [%v] in exclude list, mark as invalid",node.Http.PublishAddress)
			return false
		}
	}

	for _, v := range cfg.Filter.Hosts.Include {
		hasInclude=true
		if endpoint==v{
			log.Debugf("host [%v] in include list, mark as valid",node.Http.PublishAddress)
			return true
		}
	}

	//no exclude and only have include, means white list mode
	if !hasExclude && hasInclude{
		return false
	}


	hasExclude=false
	hasInclude =false
	for _, v := range cfg.Filter.Roles.Exclude {
		hasExclude=true
		if util.ContainsAnyInArray(v,node.Roles){
			log.Debugf("node [%v] role [%v] match exclude rule [%v], mark as invalid",node.Http.PublishAddress,node.Roles,v)
			return false
		}
	}

	for _, v := range cfg.Filter.Roles.Include {
		hasInclude=true
		if util.ContainsAnyInArray(v,node.Roles){
			log.Debugf("node [%v] role [%v] match include rule [%v], mark as valid",node.Http.PublishAddress,node.Roles,v)
			return true
		}
	}

	if !hasExclude && hasInclude{
		return false
	}

	hasExclude=false
	hasInclude =false
	for _,o := range cfg.Filter.Tags.Exclude {
		hasExclude=true
		for k,v:=range o{
			v1,ok:=node.Attributes[k]
			if ok{
				if v1==v{
					log.Debugf("node [%v] tags [%v:%v] in exclude list, mark as invalid",node.Http.PublishAddress,k,v)
					return false
				}
			}
		}
	}

	for _,o := range cfg.Filter.Tags.Include {
		hasInclude=true
		for k, v:=range o{
			v1,ok:=node.Attributes[k]
			if ok{
				if v1==v{
					log.Debugf("node [%v] tags [%v:%v] in include list, mark as valid",node.Http.PublishAddress,k,v)
					return true
				}
			}
		}
	}

	if !hasExclude && hasInclude{
		return false
	}

	return true
}

func (p *ReverseProxy) refreshNodes(force bool) {

	if global.Env().IsDebug{
		log.Trace("elasticsearch client nodes refreshing")
	}
	cfg := p.proxyConfig
	metadata := elastic.GetMetadata(cfg.Elasticsearch)

	if metadata == nil && !force {
		log.Trace("metadata is nil and not forced, skip nodes refresh")
		return
	}

	ws := []int{}
	clients := []*fasthttp.HostClient{}
	esConfig := elastic.GetConfig(cfg.Elasticsearch)
	endpoints := []string{}

	checkMetadata:=false
	if metadata != nil && len(metadata.Nodes) > 0 {
		oldV:=p.lastNodesTopologyVersion
		p.lastNodesTopologyVersion=metadata.NodesTopologyVersion

		if oldV==p.lastNodesTopologyVersion {
			if global.Env().IsDebug{
				log.Trace("metadata.NodesTopologyVersion is equal")
			}
			return
		}

		checkMetadata=true
		for _, y := range metadata.Nodes {
			if !isEndpointValid(y, cfg) {
				continue
			}

			endpoints = append(endpoints, y.Http.PublishAddress)
		}
		log.Tracef("discovery %v nodes: [%v]", len(endpoints), util.JoinArray(endpoints, ", "))
	}
	if len(endpoints)==0{
		endpoints = append(endpoints, esConfig.GetHost())
		if checkMetadata{
			log.Warnf("no valid endpoint for elasticsearch, fallback to seed: [%v]",endpoints)
		}
	}

	//
	for _, endpoint := range endpoints {
		client := &fasthttp.HostClient{
			Name: "reverse_proxy",
			Addr:                          endpoint,
			DisableHeaderNamesNormalizing: true,
			DisablePathNormalizing:        true,
			MaxConns:                      cfg.MaxConnection,
			MaxResponseBodySize:           cfg.MaxResponseBodySize,

			MaxConnWaitTimeout:  cfg.MaxConnWaitTimeout,
			MaxConnDuration:     cfg.MaxConnDuration,
			MaxIdleConnDuration: cfg.MaxIdleConnDuration,
			ReadTimeout:         cfg.ReadTimeout,
			WriteTimeout:        cfg.WriteTimeout,
			ReadBufferSize:      cfg.ReadBufferSize,
			WriteBufferSize:     cfg.WriteBufferSize,
			//RetryIf: func(request *fasthttp.Request) bool {
			//
			//},
			IsTLS: esConfig.IsTLS(),
			TLSConfig: &tls.Config{
				InsecureSkipVerify: cfg.TLSInsecureSkipVerify,
			},
		}
		clients = append(p.clients, client)
		//get predefined weights
		w, o := cfg.Weights[endpoint]
		if !o || w <= 0 {
			w = 1
		}
		ws = append(ws, w)
	}

	if len(clients) == 0 {
		log.Error("proxy upstream is empty")
		esConfig.ReportFailure()
		return
	}

	//replace with new clients
	p.clients = clients
	p.bla = balancer.NewBalancer(ws)
	log.Infof("elasticsearch [%v] endpoints: [%v] => [%v]", esConfig.Name, util.JoinArray(p.endpoints, ", "), util.JoinArray(endpoints, ", "))
	p.endpoints = endpoints
	log.Trace(esConfig.Name," elasticsearch client nodes refreshed")
}

func NewReverseProxy(cfg *ProxyConfig) *ReverseProxy {

	p := ReverseProxy{
		oldAddr:     "",
		clients:     []*fasthttp.HostClient{},
		proxyConfig: cfg,
	}

	p.refreshNodes(true)

	if cfg.Refresh.Enabled{
		log.Debugf("refresh enabled for elasticsearch: [%v]",cfg.Elasticsearch)
		task:=task2.ScheduleTask{
			Description:fmt.Sprintf("refresh nodes for elasticsearch [%v]",cfg.Elasticsearch),
			Type:"interval",
			Interval: cfg.Refresh.Interval,
			Task: func() {
				p.refreshNodes(false)
			},
		}
		task2.RegisterScheduleTask(task)
	}


	return &p
}

func (p *ReverseProxy) getClient() (clientAvailable bool,client *fasthttp.HostClient,endpoint string) {
	if p.clients == nil {
		panic("ReverseProxy has been closed")
	}

	if len(p.clients)==0{
		log.Error("no upstream found")
		return false,nil,""
	}

	if p.bla != nil {
		// bla has been opened
		idx := p.bla.Distribute()
		if idx >= len(p.clients) {
			log.Warn("invalid offset, ",idx," vs ",len(p.clients),p.clients,p.endpoints,", random pick now")
			idx = 0
			goto RANDOM
		}

		if len(p.clients)!=len(p.endpoints){
			log.Warn("clients != endpoints, ",len(p.clients)," vs ",len(p.endpoints),", random pick now")
			goto RANDOM
		}

		c := p.clients[idx]
		e:=p.endpoints[idx]
		return true,c,e
	}

	RANDOM:
	//or go random way
	max := len(p.clients)
	seed := rand.Intn(max)
	if seed >= len(p.clients) {
		log.Warn("invalid upstream offset, reset to 0")
		seed = 0
	}
	c := p.clients[seed]
	e :=p.endpoints[seed]
	return true,c,e
}

func cleanHopHeaders(req *fasthttp.Request) {
	for _, h := range hopHeaders {
		req.Header.Del(h)
	}
}

var failureMessage=[]string{"connection refused","connection reset","no such host","timed out","Connection: close"}

func (p *ReverseProxy) DelegateRequest(elasticsearch string,cfg *elastic.ElasticsearchConfig,myctx *fasthttp.RequestCtx) {

	stats.Increment("cache", "strike")

	retry:=0
	START:

	//使用算法来获取合适的 client
	ok,pc,endpoint := p.getClient()
	if !ok{
		//TODO no client available, throw error directly
	}

	req:=&myctx.Request
	res:=&myctx.Response

	cleanHopHeaders(req)

	if global.Env().IsDebug {
		log.Tracef("send request [%v] to upstream [%v]", req.URI().String(), pc.Addr)
	}


	if cfg.TrafficControl!=nil{
	RetryRateLimit:

		if cfg.TrafficControl.MaxQpsPerNode>0{
			//fmt.Println("MaxQpsPerNode:",cfg.TrafficControl.MaxQpsPerNode)
			if !rate.GetRateLimiterPerSecond(cfg.Name,endpoint+"max_qps", int(cfg.TrafficControl.MaxQpsPerNode)).Allow(){
				if global.Env().IsDebug {
					log.Tracef("throttle request [%v] to upstream [%v]", req.URI().String(), myctx.RemoteAddr().String())
				}
				time.Sleep(10*time.Millisecond)
				goto RetryRateLimit
			}
		}

		if cfg.TrafficControl.MaxBytesPerNode>0{
			//fmt.Println("MaxBytesPerNode:",cfg.TrafficControl.MaxQpsPerNode)
			if !rate.GetRateLimiterPerSecond(cfg.Name,endpoint+"max_bps", int(cfg.TrafficControl.MaxBytesPerNode)).AllowN(time.Now(),req.GetRequestLength()){
				if global.Env().IsDebug {
					log.Tracef("throttle request [%v] to upstream [%v]", req.URI().String(), myctx.RemoteAddr().String())
				}
				time.Sleep(10*time.Millisecond)
				goto RetryRateLimit
			}
		}
	}

	if err := pc.Do(req, res); err != nil {
		//if global.Env().IsDebug{
			log.Warnf("failed to proxy request: %v, %v, retried #%v", err, string(req.RequestURI()),retry)
		//}

		if util.ContainsAnyInArray(err.Error(),failureMessage){
			//record translog, update failure ticket
			if global.Env().IsDebug {
				log.Errorf("elasticsearch [%v] is on fire now", p.proxyConfig.Elasticsearch)
			}
			cfg.ReportFailure()

			//server failure flow

		}else if  res.StatusCode()==429{
				retry++
				if p.proxyConfig.maxRetryTimes>0 && retry<p.proxyConfig.maxRetryTimes {
					if p.proxyConfig.retryDelayInMs>0{
						time.Sleep(time.Duration(p.proxyConfig.retryDelayInMs)*time.Millisecond)
					}
					goto START
				}else{
					log.Debugf("reached max retries, failed to proxy request: %v, %v", err, string(req.RequestURI()))
				}
		}

		//TODO if backend failure and after reached max retry, should save translog and mark the elasticsearch cluster to downtime, deny any new requests
		// the translog file should consider to contain dirty writes, could be used to do cross cluster check or manually operations recovery.
		res.SetBody([]byte(err.Error()))
	}else{
		if global.Env().IsDebug {
			log.Tracef("request [%v] [%v] [%v]", req.URI().String(),res.StatusCode(), util.SubString(string(res.GetRawBody()),0,256))
		}
	}

	res.Header.Set("CLUSTER", p.proxyConfig.Elasticsearch)

	if myctx.Has("elastic_cluster_name"){
		es1:=myctx.MustGetStringArray("elastic_cluster_name")
		myctx.Set("elastic_cluster_name",append(es1,elasticsearch))
	}else{
		myctx.Set("elastic_cluster_name",[]string{elasticsearch})
	}

	res.Header.Set("UPSTREAM", pc.Addr)

	myctx.SetDestination(pc.Addr)

}

// SetClient ...
func (p *ReverseProxy) SetClient(addr string) *ReverseProxy {
	for idx := range p.clients {
		p.clients[idx].Addr = addr
	}
	return p
}

// Reset ...
func (p *ReverseProxy) Reset() {
	for idx := range p.clients {
		p.clients[idx].Addr = ""
	}
}

// Close ... clear and release
func (p *ReverseProxy) Close() {
	p.clients = nil
	p.bla = nil
	p = nil
}

// Hop-by-hop headers. These are removed when sent to the backend.
// As of RFC 7230, hop-by-hop headers are required to appear in the
// Connection header field. These are the headers defined by the
// obsoleted RFC 2616 (section 13.5.1) and are used for backward
// compatibility.
var hopHeaders = []string{
	"Connection",          // Connection
	"Proxy-Connection",    // non-standard but still sent by libcurl and rejected by e.g. google
	"Keep-Alive",          // Keep-Alive
	"Proxy-Authenticate",  // Proxy-Authenticate
	"Proxy-Authorization", // Proxy-Authorization
	"Te",                  // canonicalized version of "TE"
	"Trailer",             // not Trailers per URL above; https://www.rfc-editor.org/errata_search.php?eid=4522
	"Transfer-Encoding",   // Transfer-Encoding
	"Upgrade",             // Upgrade

	//"Accept-Encoding",             // Disable Gzip
	//"Content-Encoding",             // Disable Gzip
}
