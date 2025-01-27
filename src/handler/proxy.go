package handler

import (
	"aad-auth-proxy/constants"
	"aad-auth-proxy/contracts"
	"aad-auth-proxy/utils"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
)

// Creates proxy for incoming requests
func CreateReverseProxy(targetHost string, tokenProvider contracts.ITokenProvider) (*httputil.ReverseProxy, error) {
	url, err := url.Parse(targetHost)
	if err != nil {
		return nil, err
	}
	proxy := httputil.NewSingleHostReverseProxy(url)

	proxy.Director = func(request *http.Request) {
		modifyRequest(request, targetHost, tokenProvider)
	}
	proxy.ErrorHandler = handleError
	proxy.ModifyResponse = modifyResponse

	return proxy, nil
}

// This modifies incoming requests and changes host to targetHost
func modifyRequest(request *http.Request, targetHost string, tokenProvider contracts.ITokenProvider) {
	ctx, span := otel.Tracer(constants.SERVICE_TELEMETRY_KEY).Start(request.Context(), "modifyRequest")
	defer span.End()

	span.SetAttributes(
		attribute.String("request.target_scheme", constants.HTTPS_SCHEME),
		attribute.String("request.target_host", targetHost),
	)

	request.URL.Scheme = constants.HTTPS_SCHEME
	request.URL.Host = targetHost
	request.Host = targetHost

	// Record metrics
	// request_bytes_total{target_host, method, path, user_agent}
	metricAttributes := []attribute.KeyValue{
		attribute.String("target_host", request.URL.Host),
		attribute.String("method", request.Method),
		attribute.String("path", request.URL.Path),
		attribute.String("user_agent", request.Header.Get(constants.HEADER_USER_AGENT)),
	}

	meter := otel.Meter(constants.SERVICE_TELEMETRY_KEY)
	intrument, err := meter.Int64Counter(constants.METRIC_REQUEST_BYTES_TOTAL)
	if err == nil {
		options := metric.WithAttributes(metricAttributes...)
		intrument.Add(ctx, request.ContentLength, options)
	}
}

// This will be called when there is an error in forwarding the request
func handleError(response http.ResponseWriter, request *http.Request, response_err error) {
	// Record traces
	ctx, span := otel.Tracer(constants.SERVICE_TELEMETRY_KEY).Start(request.Context(), "handleError")
	defer span.End()

	attributes := []attribute.KeyValue{
		attribute.String("response.status_code", response.Header().Get(constants.HEADER_STATUS_CODE)),
		attribute.String("response.content_type", response.Header().Get(constants.HEADER_CONTENT_TYPE)),
		attribute.String("response.content_encoding", response.Header().Get(constants.HEADER_CONTENT_ENCODING)),
		attribute.String("response.request_id", response.Header().Get(constants.HEADER_REQUEST_ID)),
		attribute.String("response.error.message", response_err.Error()),
	}

	span.SetAttributes(attributes...)
	span.RecordError(response_err)
	span.SetStatus(codes.Error, "failed to forward request")

	// Log error
	log.WithFields(log.Fields{
		"Request": request.URL.String(),
	}).Errorln("Request failed", response_err)

	// Record metrics
	// requests_total{target_host, method, path, user_agent, status_code}
	status_code, err := strconv.ParseInt(response.Header().Get(constants.HEADER_STATUS_CODE), 10, 32)
	if err != nil {
		log.Errorln("Failed to parse status code, returning status code 503")
		status_code = http.StatusServiceUnavailable
	}

	metricAttributes := []attribute.KeyValue{
		attribute.String("target_host", request.URL.Host),
		attribute.String("method", request.Method),
		attribute.String("path", request.URL.Path),
		attribute.String("user_agent", request.Header.Get(constants.HEADER_USER_AGENT)),
		attribute.Int("status_code", int(status_code)),
	}

	requestCountMeter := otel.Meter(constants.SERVICE_TELEMETRY_KEY)
	requestCountIntrument, err := requestCountMeter.Int64Counter(constants.METRIC_REQUESTS_TOTAL)
	if err == nil {
		options := metric.WithAttributes(metricAttributes...)
		requestCountIntrument.Add(ctx, 1, options)
	}

	FailRequest(response, request, int(status_code), response_err.Error(), ctx, response_err)
}

// This will be called once we receive response from targetHost
func modifyResponse(response *http.Response) (err error) {
	// Record traces
	ctx, span := otel.Tracer(constants.SERVICE_TELEMETRY_KEY).Start(response.Request.Context(), "modifyResponse")
	defer span.End()

	traceAttributes := []attribute.KeyValue{
		attribute.Int("response.status_code", response.StatusCode),
		attribute.String("response.content_length", response.Header.Get(constants.HEADER_CONTENT_LENGTH)),
		attribute.String("response.content_type", response.Header.Get(constants.HEADER_CONTENT_TYPE)),
		attribute.String("response.content_encoding", response.Header.Get(constants.HEADER_CONTENT_ENCODING)),
		attribute.String("response.request_id", response.Header.Get(constants.HEADER_REQUEST_ID)),
	}

	span.SetAttributes(traceAttributes...)

	// Metric attributes
	metricAttributes := []attribute.KeyValue{
		attribute.String("target_host", response.Request.URL.Host),
		attribute.String("method", response.Request.Method),
		attribute.String("path", response.Request.URL.Path),
		attribute.String("user_agent", response.Request.Header.Get(constants.HEADER_USER_AGENT)),
		attribute.Int("status_code", response.StatusCode),
	}

	// Record metrics
	// requests_total{target_host, method, path, user_agent, status_code}
	requestCountMeter := otel.Meter(constants.SERVICE_TELEMETRY_KEY)
	requestCountIntrument, err := requestCountMeter.Int64Counter(constants.METRIC_REQUESTS_TOTAL)
	if err == nil {
		options := metric.WithAttributes(metricAttributes...)
		requestCountIntrument.Add(ctx, 1, options)
	}

	// Record metrics
	// response_bytes_total{target_host, method, path, user_agent, status_code}
	responseBytesMeter := otel.Meter(constants.SERVICE_TELEMETRY_KEY)
	responseBytesIntrument, err := responseBytesMeter.Int64Counter(constants.METRIC_RESPONSE_BYTES_TOTAL)
	if err == nil {
		options := metric.WithAttributes(metricAttributes...)
		responseBytesIntrument.Add(ctx, response.ContentLength, options)
	}

	// Log response
	log.WithFields(log.Fields{
		"Request":       response.Request.URL.String(),
		"StatusCode":    response.StatusCode,
		"ContentLength": response.ContentLength,
		"RequestID":     response.Header.Get(constants.HEADER_REQUEST_ID),
	}).Infoln("Successfully sent request, returning response back.")

	response.Header.Set("Status-Code", strconv.Itoa(response.StatusCode))

	// If server returned error, log response as well
	if response.StatusCode >= http.StatusBadRequest {
		err = errors.New("Non 2xx response from target host: " + strconv.Itoa(response.StatusCode))
		span.RecordError(err)
		span.SetStatus(codes.Error, "Non 2xx response from target host")

		logResponse(ctx, response)
	}

	return nil
}

// This will check encoding, if encoding is gzip or deflate, it will decode response body and log it
func logResponse(ctx context.Context, response *http.Response) {
	var responseBody []byte
	var err error
	var buffer bytes.Buffer

	encoding := response.Header.Get(constants.HEADER_CONTENT_ENCODING)
	encoderDecoder := utils.NewEncoderDecoder()

	responseBody, err = encoderDecoder.Decode(encoding, response.Body)
	if err != nil {
		log.Errorln("Failed to decode response body", err)
		return
	}

	log.WithFields(log.Fields{
		"Encoding": encoding,
	}).Errorln("Error response body: ", string(responseBody[:]))

	buffer, err = encoderDecoder.Encode(encoding, responseBody)
	if err != nil {
		log.Errorln("Failed to encode response body", err)
		return
	}

	// Set response body back
	response.Body = ioutil.NopCloser(bytes.NewReader(buffer.Bytes()))

	// Set all headers back
	response.ContentLength = int64(buffer.Len())
	response.Header.Set(constants.HEADER_CONTENT_LENGTH, fmt.Sprint(buffer.Len()))
	response.Header.Set(constants.HEADER_CONTENT_ENCODING, encoding)
}
