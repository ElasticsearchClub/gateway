package throttle

import (
	"fmt"
	log "github.com/cihub/seelog"
	"infini.sh/framework/core/config"
	"infini.sh/framework/core/pipeline"
	"infini.sh/framework/core/queue"
	"infini.sh/framework/core/util"
	"infini.sh/framework/lib/fasthttp"
)

type RetryLimiter struct {
	MaxRetryTimes int `config:"max_retry_times"`
	Queue string `config:"queue_name"`
}

func (filter *RetryLimiter) Name() string {
	return "retry_limiter"
}

const RetryKey = "Retried_times"

func (filter *RetryLimiter) Filter(ctx *fasthttp.RequestCtx) {

	timeBytes:=ctx.Request.Header.Peek(RetryKey)
	times:=0
	if timeBytes!=nil{
		t,err:=util.ToInt(string(timeBytes))
		if err==nil{
			times=t
		}
	}

	if times>filter.MaxRetryTimes{
		log.Debugf("hit max retry times")
		ctx.Finished()
		ctx.Request.Header.Del(RetryKey)
		queue.Push(filter.Queue,ctx.Request.Encode())
		return
	}

	times++
	ctx.Request.Header.Set(RetryKey,util.IntToString(times))
}


func NewRetryLimiter(c *config.Config) (pipeline.Filter, error) {

	runner := RetryLimiter{
		MaxRetryTimes: 3,
	}
	if err := c.Unpack(&runner); err != nil {
		return nil, fmt.Errorf("failed to unpack the filter configuration : %s", err)
	}

	return &runner, nil
}