/* ©INFINI, All Rights Reserved.
 * mail: contact#infini.ltd */

package elastic

import (
	"fmt"
	log "github.com/cihub/seelog"
	"infini.sh/framework/core/config"
	"infini.sh/framework/core/elastic"
	"infini.sh/framework/core/pipeline"
	"infini.sh/framework/core/queue"
	"infini.sh/framework/core/rate"
	"infini.sh/framework/core/stats"
	"infini.sh/framework/core/util"
	"infini.sh/framework/lib/bytebufferpool"
	"infini.sh/framework/lib/fasthttp"
	"infini.sh/gateway/common"
	"net/http"
	"path"
	"time"
)

type BulkResponseProcess struct {
	config    *Config
	retryFlow *common.FilterFlow
}

func (this *BulkResponseProcess) Name() string {
	return "bulk_response_process"
}

func (this *BulkResponseProcess) Filter(ctx *fasthttp.RequestCtx) {
	path := string(ctx.URI().Path())
	if string(ctx.Request.Header.Method()) != "POST" || !util.ContainStr(path, "_bulk") {
		return
	}

	if ctx.Response.StatusCode() == http.StatusOK || ctx.Response.StatusCode() == http.StatusCreated {
		var resbody = ctx.Response.GetRawBody()
		requestBytes := ctx.Request.GetRawBody()

		nonRetryableItems := bytebufferpool.Get("bulk_request_docs")
		retryableItems := bytebufferpool.Get("bulk_request_docs")
		successItems := bytebufferpool.Get("bulk_request_docs")

		defer bytebufferpool.Put("bulk_request_docs", nonRetryableItems)
		defer bytebufferpool.Put("bulk_request_docs", retryableItems)
		defer bytebufferpool.Put("bulk_request_docs", successItems)

		containError := this.HandleBulkResponse(ctx, this.config.SafetyParse, requestBytes, resbody, this.config.DocBufferSize, nonRetryableItems, retryableItems, successItems)
		if containError {

			url := ctx.Request.URI().String()
			if rate.GetRateLimiter("bulk_error", url, 1, 1, 5*time.Second).Allow() {
				log.Error("error in bulk requests,", url, ",", ctx.Response.StatusCode(), ",", util.SubString(util.UnsafeBytesToString(resbody), 0, this.config.MessageTruncateSize))
			}

			if len(this.config.TagsOnAnyError) > 0 {
				ctx.UpdateTags(this.config.TagsOnAnyError, nil)
			}

			if nonRetryableItems.Len() > 0 {

				if this.config.InvalidQueue != "" {
					nonRetryableItems.WriteByte('\n')
					bytes := ctx.Request.OverrideBodyEncode(nonRetryableItems.Bytes(), true)
					queue.Push(queue.GetOrInitConfig(this.config.InvalidQueue), bytes)

					queue.Push(queue.GetOrInitConfig(this.config.InvalidQueue+"-bulk-error-messages"), util.MustToJSONBytes(
						util.MapStr{
							"request": util.MapStr{
								"uri":  ctx.Request.URI().String(),
								"body": util.SubString(util.UnsafeBytesToString(ctx.Request.GetRawBody()), 0, 1024*4),
							},
							"response": util.MapStr{
								"status": ctx.Response.StatusCode(),
								"body":   util.SubString(util.UnsafeBytesToString(ctx.Response.GetRawBody()), 0, 1024*4),
							},
						}))
				}

				stats.IncrementBy("bulk_response", "invalid_unretry_items", int64(nonRetryableItems.Len()))

				if len(this.config.TagsOnPartialInvalid) > 0 {
					ctx.UpdateTags(this.config.TagsOnPartialInvalid, nil)
				}

				if successItems.Len() == 0 && retryableItems.Len() == 0 {
					if len(this.config.TagsOnAllInvalid) > 0 {
						ctx.UpdateTags(this.config.TagsOnAllInvalid, nil)
					}
				}
			}

			if retryableItems.Len() > 0 {

				if this.config.FailureQueue != "" {
					retryableItems.WriteByte('\n')
					bytes := ctx.Request.OverrideBodyEncode(retryableItems.Bytes(), true)

					if this.config.PartialFailureRetry && this.retryFlow != nil {
						ctx.AddFlowProcess("retry_flow:" + this.retryFlow.ID)
						this.retryFlow.Process(ctx)
					}

					queue.Push(queue.GetOrInitConfig(this.config.FailureQueue), bytes)
				}

				stats.IncrementBy("bulk_response", "failure_retry_items", int64(retryableItems.Len()))

				if len(this.config.TagsOnPartialFailure) > 0 {
					ctx.UpdateTags(this.config.TagsOnPartialFailure, nil)
				}

				if successItems.Len() == 0 && nonRetryableItems.Len() == 0 {
					if len(this.config.TagsOnAllFailure) > 0 {
						ctx.UpdateTags(this.config.TagsOnAllFailure, nil)
					}
				}
			}

			if successItems.Len() > 0 {

				if this.config.SuccessQueue != "" {
					successItems.WriteByte('\n')
					bytes := ctx.Request.OverrideBodyEncode(successItems.Bytes(), true)
					queue.Push(queue.GetOrInitConfig(this.config.SuccessQueue), bytes)
				}

				stats.IncrementBy("bulk_response", "partial_success_items", int64(successItems.Len()))

				if len(this.config.TagsOnPartialSuccess) > 0 {
					ctx.UpdateTags(this.config.TagsOnPartialSuccess, nil)
				}
			}

			//出错不继续交由后续流程，直接结束处理
			if !this.config.ContinueOnAnyError {
				//log.Errorf("this.config.ContinueOnError:%v, %v",this.config.ContinueOnAnyError,ctx.GetFlowProcess())
				ctx.Finished()
				return
			}
		} else {
			//没有错误，标记处理完成
			if len(this.config.TagsOnAllSuccess) > 0 {
				ctx.UpdateTags(this.config.TagsOnAllSuccess, nil)
			}

			if this.config.SuccessQueue != "" {
				queue.Push(queue.GetOrInitConfig(this.config.SuccessQueue), ctx.Request.Encode())
			}

			if !this.config.ContinueOnSuccess {
				ctx.Finished()
				return
			}
		}
	} else {

		if len(this.config.TagsOnNone2xx) > 0 {
			ctx.UpdateTags(this.config.TagsOnNone2xx, nil)
		}

		queue.Push(queue.GetOrInitConfig(this.config.InvalidQueue+"-req-error-messages"), util.MustToJSONBytes(
			util.MapStr{
				"context": ctx.GetFlowProcess(),
				"request": util.MapStr{
					"uri":  ctx.Request.URI().String(),
					"body": util.SubString(util.UnsafeBytesToString(ctx.Request.GetRawBody()), 0, 1024*4),
				},
				"response": util.MapStr{
					"status": ctx.Response.StatusCode(),
					"body":   util.SubString(util.UnsafeBytesToString(ctx.Response.GetRawBody()), 0, 1024*4),
				},
			}))

		if this.config.FailureQueue != "" {
			queue.Push(queue.GetOrInitConfig(this.config.FailureQueue), ctx.Request.Encode())
		}

		if !this.config.ContinueOnAllError {
			ctx.Finished()
			return
		}
	}
}

func (this *BulkResponseProcess) HandleBulkResponse(ctx *fasthttp.RequestCtx, safetyParse bool, requestBytes, resbody []byte, docBuffSize int, nonRetryableItems, retryableItems, successItems *bytebufferpool.ByteBuffer) bool {
	containError := util.LimitedBytesSearch(resbody, []byte("\"errors\":true"), 64)
	if containError {
		//decode response
		response := elastic.BulkResponse{}
		err := response.UnmarshalJSON(resbody)
		if err != nil {
			panic(err)
		}
		invalidOffset := map[int]elastic.BulkActionMetadata{}
		var validCount = 0
		var statsCodeStats = map[int]int{}
		for i, v := range response.Items {
			item := v.GetItem()

			x, ok := statsCodeStats[item.Status]
			if !ok {
				x = 0
			}
			x++
			statsCodeStats[item.Status] = x

			if item.Error != nil {
				invalidOffset[i] = v
			} else {
				validCount++
			}
		}
		if len(invalidOffset) > 0 {
			log.Debug("bulk status:", statsCodeStats)
		}

		for x, y := range statsCodeStats {
			stats.IncrementBy(path.Join("request_flow", ctx.GetFlowIDOrDefault("flow")), fmt.Sprintf("bulk_items_response.%v", x), int64(y))
		}

		ctx.Set("bulk_response_status", statsCodeStats)
		ctx.Response.Header.Set("X-BulkRequest-Failed", "true")

		var offset = 0
		var match = false
		var retryable = false
		var actionMetadata elastic.BulkActionMetadata
		var docBuffer []byte
		docBuffer = elastic.BulkDocBuffer.Get(docBuffSize)
		defer elastic.BulkDocBuffer.Put(docBuffer)

		elastic.WalkBulkRequests(safetyParse, requestBytes, docBuffer, func(eachLine []byte) (skipNextLine bool) {
			return false
		}, func(metaBytes []byte, actionStr, index, typeName, id string) (err error) {
			actionMetadata, match = invalidOffset[offset]
			if match {

				//find invalid request
				if actionMetadata.GetItem().Status >= 400 && actionMetadata.GetItem().Status < 500 && actionMetadata.GetItem().Status != 429 {
					retryable = false
					//contains400Error = true
					if nonRetryableItems.Len() > 0 {
						nonRetryableItems.WriteByte('\n')
					}
					nonRetryableItems.Write(metaBytes)
				} else {
					retryable = true
					if retryableItems.Len() > 0 {
						retryableItems.WriteByte('\n')
					}
					retryableItems.Write(metaBytes)
				}
			} else {
				if successItems.Len() > 0 {
					successItems.WriteByte('\n')
				}
				successItems.Write(metaBytes)
			}

			offset++
			return nil
		}, func(payloadBytes []byte) {
			if match {
				if payloadBytes != nil && len(payloadBytes) > 0 {
					if retryable {
						if retryableItems.Len() > 0 {
							retryableItems.WriteByte('\n')
						}
						retryableItems.Write(payloadBytes)
					} else {
						if nonRetryableItems.Len() > 0 {
							nonRetryableItems.WriteByte('\n')
						}
						nonRetryableItems.Write(payloadBytes)
					}
				}
			} else {
				if successItems.Len() > 0 {
					successItems.WriteByte('\n')
				}
				successItems.Write(payloadBytes)
			}
		})

	}
	return containError
}

type Config struct {
	SafetyParse bool `config:"safety_parse"`

	DocBufferSize int `config:"doc_buffer_size"`

	SuccessQueue string `config:"success_queue"`
	InvalidQueue string `config:"invalid_queue"`
	FailureQueue string `config:"failure_queue"`

	MessageTruncateSize int `config:"message_truncate_size"`

	PartialFailureRetry                 bool `config:"partial_failure_retry"`               //是否主动重试，只有部分失败的请求，避免大量没有意义的 409
	PartialFailureMaxRetryTimes         int  `config:"partial_failure_max_retry_times"`     //是否主动重试，只有部分失败的请求，避免大量没有意义的 409
	PartialFailureRetryDelayLatencyInMs int  `config:"partial_failure_retry_latency_in_ms"` //是否主动重试，只有部分失败的请求，避免大量没有意义的 409

	ContinueOnAllError bool `config:"continue_on_all_error"` //没有拿到响应，整个请求都失败是否继续处理后续 flow
	ContinueOnAnyError bool `config:"continue_on_any_error"` //拿到响应，出现任意请求失败是否都继续 flow 还是结束处理
	ContinueOnSuccess  bool `config:"continue_on_success"`   //所有请求都成功

	TagsOnAllSuccess []string `config:"tag_on_all_success"` //所有请求都成功，没有失败
	TagsOnNone2xx    []string `config:"tag_on_none_2xx"`    //整个 bulk 请求非 200 或者 201 返回

	//bulk requests
	TagsOnAllInvalid []string `config:"tag_on_all_invalid"` //所有请求都是非法请求的情况
	TagsOnAllFailure []string `config:"tag_on_all_failure"` //所有失败的请求都是失败请求的情况

	TagsOnAnyError       []string `config:"tag_on_any_error"`       //请求里面包含任意失败或者非法请求的情况
	TagsOnPartialSuccess []string `config:"tag_on_partial_success"` //包含部分成功的情况
	TagsOnPartialFailure []string `config:"tag_on_partial_failure"` //包含部分失败的情况，可以重试
	TagsOnPartialInvalid []string `config:"tag_on_partial_invalid"` //包含部分非法请求的情况，无需重试的请求

	RetryFlow string `config:"retry_flow"`
}

func init() {
	pipeline.RegisterFilterPluginWithConfigMetadata("bulk_response_process", NewBulkResponseValidate, &Config{})
}

func NewBulkResponseValidate(c *config.Config) (pipeline.Filter, error) {
	cfg := Config{
		DocBufferSize:       256 * 1024,
		SafetyParse:         true,
		MessageTruncateSize: 1024,
	}
	if err := c.Unpack(&cfg); err != nil {
		return nil, fmt.Errorf("failed to unpack the filter configuration : %s", err)
	}
	runner := BulkResponseProcess{config: &cfg}

	if runner.config.RetryFlow != "" && runner.config.PartialFailureRetry {
		flow := common.MustGetFlow(runner.config.RetryFlow)
		runner.retryFlow = &flow
	}

	return &runner, nil
}
