package main

import (
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

func TestMetricsMiddleware(t *testing.T) {
	// Reset the global request count before test
	RequestCount = 0

	handler := MetricsMiddleware(func(ctx *fasthttp.RequestCtx) {
		ctx.SetStatusCode(fasthttp.StatusOK)
	})

	ctx := &fasthttp.RequestCtx{}

	// First request
	handler(ctx)
	if RequestCount != 1 {
		t.Errorf("Expected RequestCount to be 1, got %d", RequestCount)
	}

	// Second request
	handler(ctx)
	if RequestCount != 2 {
		t.Errorf("Expected RequestCount to be 2, got %d", RequestCount)
	}
}

func TestRequestHandler_NoSignature(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/proxy")

	RequestHandler(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusPaymentRequired {
		t.Errorf("Expected 402 Payment Required, got %d", ctx.Response.StatusCode())
	}
}

func TestSolanaFailover(t *testing.T) {
	// Start a mock failing backend server
	lnFailing, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to start failing mock server: %v", err)
	}
	failingAddr := lnFailing.Addr().String()

	mockFailingHandler := func(ctx *fasthttp.RequestCtx) {
		ctx.SetStatusCode(fasthttp.StatusInternalServerError)
		ctx.SetBodyString("Internal Server Error")
	}
	go fasthttp.Serve(lnFailing, mockFailingHandler)
	defer lnFailing.Close()

	// Start a mock succeeding backend server
	lnSucceeding, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to start succeeding mock server: %v", err)
	}
	succeedingAddr := lnSucceeding.Addr().String()

	mockSucceedingHandler := func(ctx *fasthttp.RequestCtx) {
		ctx.SetStatusCode(fasthttp.StatusOK)
		ctx.SetBodyString("Mock solana response")
	}
	go fasthttp.Serve(lnSucceeding, mockSucceedingHandler)
	defer lnSucceeding.Close()

	// Wait for servers to start
	time.Sleep(100 * time.Millisecond)

	// Set endpoints, first the failing one, then the succeeding one
	solanaEndpoints = []string{"http://" + failingAddr, "http://" + succeedingAddr}

	// Reset solanaEndpointIndex so that the next request hits the failing endpoint first.
	// We want (startIdx + 0) % 2 == 0, where startIdx is the incremented value.
	// If solanaEndpointIndex is set to 1, startIdx will be 2.
	// (2 + 0) % 2 == 0.
	solanaEndpointIndex = 1

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("POST")
	ctx.Request.SetRequestURI("/solana")
	ctx.Request.SetBodyString(`{"jsonrpc":"2.0","id":1, "method":"getHealth"}`)

	RequestHandler(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Errorf("Expected 200 OK after failover, got %d", ctx.Response.StatusCode())
	}

	if string(ctx.Response.Body()) != "Mock solana response" {
		t.Errorf("Expected Mock solana response, got %s", string(ctx.Response.Body()))
	}
}

func TestRequestHandler_WithSignature(t *testing.T) {
	// Start a mock upstream server
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to start mock server: %v", err)
	}

	upstreamAddr := ln.Addr().String()

	mockHandler := func(ctx *fasthttp.RequestCtx) {
		// Assert that X-Proxy-Auth header is present
		if string(ctx.Request.Header.Peek("X-Proxy-Auth")) != "test-token" {
			t.Errorf("Expected X-Proxy-Auth to be test-token, got %s", ctx.Request.Header.Peek("X-Proxy-Auth"))
		}

		if string(ctx.Request.Header.Peek("Signature")) != "valid-sig-format" {
			t.Errorf("Expected Signature to be valid-sig-format, got %s", ctx.Request.Header.Peek("Signature"))
		}

		ctx.SetStatusCode(fasthttp.StatusOK)
		ctx.SetBodyString("Mock upstream response")
	}

	go fasthttp.Serve(ln, mockHandler)
	defer ln.Close()

	// Wait for server to start
	time.Sleep(100 * time.Millisecond)

	// Set environment variables for the proxy
	os.Setenv("SETTLEMENT_CORE_URL", "http://"+upstreamAddr)
	os.Setenv("PROXY_AUTH_TOKEN", "test-token")
	defer os.Unsetenv("SETTLEMENT_CORE_URL")
	defer os.Unsetenv("PROXY_AUTH_TOKEN")

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/proxy")
	ctx.Request.Header.Set("Signature", "valid-sig-format")

	RequestHandler(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Errorf("Expected 200 OK, got %d", ctx.Response.StatusCode())
	}

	if string(ctx.Response.Body()) != "Mock upstream response" {
		t.Errorf("Expected Mock upstream response, got %s", string(ctx.Response.Body()))
	}
}

func TestMetricsEndpoint(t *testing.T) {
	// Set arbitrary initial values for metrics
	atomic.StoreUint64(&SolanaRPCRequestCount, 5)
	atomic.StoreUint64(&SolanaRPCSuccessCount, 3)
	atomic.StoreUint64(&SolanaRPCErrorCount, 1)
	atomic.StoreUint64(&SolanaRPCRetryCount, 2)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/metrics")

	RequestHandler(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Errorf("Expected 200 OK, got %d", ctx.Response.StatusCode())
	}

	expectedBody := `{"solana_rpc_requests":5, "solana_rpc_success":3, "solana_rpc_errors":1, "solana_rpc_retries":2}`
	if string(ctx.Response.Body()) != expectedBody {
		t.Errorf("Expected body %s, got %s", expectedBody, string(ctx.Response.Body()))
	}
}
