package elastic

import (
	"infini.sh/framework/core/param"
	"infini.sh/framework/lib/fasthttp"
	"sync"
)

type Elasticsearch struct {
	param.Parameters
}

func (filter Elasticsearch) Name() string {
	return "elasticsearch"
}

var proxyList = map[string]*ReverseProxy{}

var (
	proxyConfig = ProxyConfig{
		MaxConnection: 1000,
	}
)

var initLock sync.Mutex

func (filter Elasticsearch) Process(ctx *fasthttp.RequestCtx) {
	var instance *ReverseProxy

	esRef := filter.GetStringOrDefault("elasticsearch", "default")

	instance, ok := proxyList[esRef]
	if !ok || instance == nil {
		initLock.Lock()
		defer initLock.Unlock()
		instance, ok = proxyList[esRef]
		//double check
		if !ok || instance == nil {

			proxyConfig.Elasticsearch = esRef
			proxyConfig.Balancer = filter.GetStringOrDefault("balancer", "weight")
			proxyConfig.MaxResponseBodySize = filter.GetIntOrDefault("max_response_size", 100*1024*1024)
			proxyConfig.MaxConnection = filter.GetIntOrDefault("max_connection", 1000)
			proxyConfig.TLSInsecureSkipVerify = filter.GetBool("tls_insecure_skip_verify", true)

			proxyConfig.ReadBufferSize = filter.GetIntOrDefault("read_buffer_size", 4096*4)
			proxyConfig.WriteBufferSize = filter.GetIntOrDefault("write_buffer_size", 4096*4)

			proxyConfig.MaxConnWaitTimeout = filter.GetDurationOrDefault("max_conn_wait_timeout", "0s")
			proxyConfig.MaxIdleConnDuration = filter.GetDurationOrDefault("max_idle_conn_duration", "10s")
			proxyConfig.MaxConnDuration = filter.GetDurationOrDefault("max_conn_duration", "0s")
			proxyConfig.ReadTimeout = filter.GetDurationOrDefault("read_timeout", "0s")
			proxyConfig.WriteTimeout = filter.GetDurationOrDefault("write_timeout", "0s")

			if filter.Has("filter") {
				filter.Config("filter", &proxyConfig.Filter)
			}

			if filter.Has("refresh") {
				filter.Config("refresh", &proxyConfig.Refresh)
			}

			instance = NewReverseProxy(&proxyConfig)
			proxyList[esRef] = instance
		}
	}

	instance.DelegateRequest(&ctx.Request, &ctx.Response)
}