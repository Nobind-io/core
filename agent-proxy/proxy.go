package main

import (
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/valyala/fasthttp"
)

var RequestCount uint64
var solanaEndpoints []string
var solanaEndpointIndex uint64

// Telemetry Metrics
var SolanaRPCRequestCount uint64
var SolanaRPCSuccessCount uint64
var SolanaRPCErrorCount uint64
var SolanaRPCRetryCount uint64

func init() {
	endpointsEnv := os.Getenv("SOLANA_RPC_ENDPOINTS")
	if endpointsEnv != "" {
		endpoints := strings.Split(endpointsEnv, ",")
		for _, ep := range endpoints {
			ep = strings.TrimSpace(ep)
			if ep != "" {
				solanaEndpoints = append(solanaEndpoints, ep)
			}
		}
	}
	if len(solanaEndpoints) == 0 {
		solanaEndpoints = []string{"https://api.mainnet-beta.solana.com"}
	}
}

func handleSolanaRequest(ctx *fasthttp.RequestCtx) {
	atomic.AddUint64(&SolanaRPCRequestCount, 1)

	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)

	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)

	ctx.Request.CopyTo(req)

	path := string(ctx.Path())
	if strings.HasPrefix(path, "/solana") {
		path = strings.TrimPrefix(path, "/solana")
		if path == "" {
			path = "/"
		}
	}

	numEndpoints := uint64(len(solanaEndpoints))
	startIdx := atomic.AddUint64(&solanaEndpointIndex, 1)

	maxRetries := 2
	baseBackoff := 100 * time.Millisecond

	for i := uint64(0); i < numEndpoints; i++ {
		idx := (startIdx + i) % numEndpoints
		endpoint := solanaEndpoints[idx]

		req.SetRequestURI(endpoint + path)
		if len(ctx.Request.URI().QueryString()) > 0 {
			req.URI().SetQueryStringBytes(ctx.Request.URI().QueryString())
		}

		req.Header.SetHostBytes(req.URI().Host())

		for attempt := 0; attempt <= maxRetries; attempt++ {
			if attempt > 0 {
				atomic.AddUint64(&SolanaRPCRetryCount, 1)
				time.Sleep(baseBackoff * time.Duration(1<<uint(attempt-1)))
				log.Printf("Solana RPC retry: endpoint=%s, attempt=%d", endpoint, attempt)
			}

			err := fasthttp.Do(req, resp)
			if err == nil && resp.StatusCode() < 500 {
				atomic.AddUint64(&SolanaRPCSuccessCount, 1)
				resp.CopyTo(&ctx.Response)
				return
			}

			log.Printf("Solana RPC error: endpoint=%s, attempt=%d, err=%v, status=%d", endpoint, attempt, err, resp.StatusCode())
		}

		log.Printf("Solana RPC failover: %s failed after %d retries", endpoint, maxRetries)
	}

	atomic.AddUint64(&SolanaRPCErrorCount, 1)
	ctx.SetStatusCode(fasthttp.StatusBadGateway)
	ctx.SetBodyString("Bad Gateway: All Solana RPC endpoints failed")
}

func MetricsMiddleware(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		atomic.AddUint64(&RequestCount, 1)
		// log.Printf("Request count: %d", count)
		next(ctx)
	}
}

// RequestHandler handles the HTTP requests.
func RequestHandler(ctx *fasthttp.RequestCtx) {
	if string(ctx.Path()) == "/health" {
		ctx.SetStatusCode(fasthttp.StatusOK)
		ctx.SetContentType("application/json")
		ctx.SetBodyString(`{"status":"ok"}`)
		return
	}

	if string(ctx.Path()) == "/metrics" {
		reqCount := atomic.LoadUint64(&SolanaRPCRequestCount)
		successCount := atomic.LoadUint64(&SolanaRPCSuccessCount)
		errCount := atomic.LoadUint64(&SolanaRPCErrorCount)
		retryCount := atomic.LoadUint64(&SolanaRPCRetryCount)

		metricsJSON := fmt.Sprintf(`{"solana_rpc_requests":%d, "solana_rpc_success":%d, "solana_rpc_errors":%d, "solana_rpc_retries":%d}`,
			reqCount, successCount, errCount, retryCount)

		ctx.SetStatusCode(fasthttp.StatusOK)
		ctx.SetContentType("application/json")
		ctx.SetBodyString(metricsJSON)
		return
	}

	if strings.HasPrefix(string(ctx.Path()), "/solana") {
		handleSolanaRequest(ctx)
		return
	}

	if string(ctx.Path()) == "/proxy" || string(ctx.Path()) == "/x402" {
		// Check for 402 Payment Required condition
		// We expect a "Signature" header for ecrecover validation
		sigHeader := ctx.Request.Header.Peek("Signature")
		if len(sigHeader) == 0 {
			ctx.SetStatusCode(fasthttp.StatusPaymentRequired)
			ctx.SetBodyString("Payment Required: Missing Signature header")
			return
		}

		sigHex := string(sigHeader)
		sigHex = strings.TrimPrefix(sigHex, "0x")
		sigBytes, err := hex.DecodeString(sigHex)
		if err != nil || len(sigBytes) != 65 {
			ctx.SetStatusCode(fasthttp.StatusPaymentRequired)
			ctx.SetBodyString("Payment Required: Invalid Signature Format")
			return
		}

		// Adjust V value to expected standard (0 or 1)
		if sigBytes[64] >= 27 {
			sigBytes[64] -= 27
		}

		// Calculate keccak256 hash of the request body
		payloadHash := crypto.Keccak256(ctx.Request.Body())

		_, err = ValidateSignature(payloadHash, sigBytes)
		if err != nil {
			log.Printf("Invalid signature: %v", err)
			ctx.SetStatusCode(fasthttp.StatusPaymentRequired)
			ctx.SetBodyString("Payment Required: Signature Validation Failed")
			return
		}

		// Forward the request to the upstream core service
		upstreamURL := os.Getenv("SETTLEMENT_CORE_URL")
		if upstreamURL == "" {
			upstreamURL = "http://localhost:8080"
		}

		proxyAuthToken := os.Getenv("PROXY_AUTH_TOKEN")

		req := fasthttp.AcquireRequest()
		defer fasthttp.ReleaseRequest(req)

		resp := fasthttp.AcquireResponse()
		defer fasthttp.ReleaseResponse(resp)

		ctx.Request.CopyTo(req)
		req.SetRequestURI(upstreamURL + string(ctx.Path()))

		if proxyAuthToken != "" {
			req.Header.Set("X-Proxy-Auth", proxyAuthToken)
		}

		err = fasthttp.Do(req, resp)
		if err != nil {
			log.Printf("Proxy error: %v", err)
			ctx.SetStatusCode(fasthttp.StatusBadGateway)
			ctx.SetBodyString("Bad Gateway")
			return
		}

		resp.CopyTo(&ctx.Response)
		return
	}

	ctx.SetStatusCode(fasthttp.StatusNotFound)
	ctx.SetBodyString("Not Found")
}

// ValidateSignature uses go-ethereum crypto to validate ecrecover
func ValidateSignature(hash []byte, sig []byte) (*ecdsa.PublicKey, error) {
	pub, err := crypto.SigToPub(hash, sig)
	if err != nil {
		return nil, err
	}
	return pub, nil
}

func main() {
	log.Println("Starting agent-proxy on :8080...")
	if err := fasthttp.ListenAndServe(":8080", MetricsMiddleware(RequestHandler)); err != nil {
		log.Fatalf("Error in ListenAndServe: %v", err)
	}
}
