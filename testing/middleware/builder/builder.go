package builder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	gethTypes "github.com/ethereum/go-ethereum/core/types"
	gethRPC "github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/trie"
	builderAPI "github.com/prysmaticlabs/prysm/v5/api/client/builder"
	"github.com/prysmaticlabs/prysm/v5/api/server/structs"
	"github.com/prysmaticlabs/prysm/v5/beacon-chain/core/signing"
	fieldparams "github.com/prysmaticlabs/prysm/v5/config/fieldparams"
	"github.com/prysmaticlabs/prysm/v5/config/params"
	"github.com/prysmaticlabs/prysm/v5/consensus-types/blocks"
	"github.com/prysmaticlabs/prysm/v5/consensus-types/interfaces"
	types "github.com/prysmaticlabs/prysm/v5/consensus-types/primitives"
	"github.com/prysmaticlabs/prysm/v5/crypto/bls"
	"github.com/prysmaticlabs/prysm/v5/encoding/bytesutil"
	"github.com/prysmaticlabs/prysm/v5/network"
	"github.com/prysmaticlabs/prysm/v5/network/authorization"
	v1 "github.com/prysmaticlabs/prysm/v5/proto/engine/v1"
	eth "github.com/prysmaticlabs/prysm/v5/proto/prysm/v1alpha1"
	"github.com/sirupsen/logrus"
)

const (
	statusPath   = "GET /eth/v1/builder/status"
	registerPath = "POST /eth/v1/builder/validators"
	headerPath   = "GET /eth/v1/builder/header/{slot}/{parent_hash}/{pubkey}"
	blindedPath  = "POST /eth/v1/builder/blinded_blocks"

	// ForkchoiceUpdatedMethod v1 request string for JSON-RPC.
	ForkchoiceUpdatedMethod = "engine_forkchoiceUpdatedV1"
	// ForkchoiceUpdatedMethodV2 v2 request string for JSON-RPC.
	ForkchoiceUpdatedMethodV2 = "engine_forkchoiceUpdatedV2"
	// ForkchoiceUpdatedMethodV3 v3 request string for JSON-RPC.
	ForkchoiceUpdatedMethodV3 = "engine_forkchoiceUpdatedV3"
	// GetPayloadMethod v1 request string for JSON-RPC.
	GetPayloadMethod = "engine_getPayloadV1"
	// GetPayloadMethodV2 v2 request string for JSON-RPC.
	GetPayloadMethodV2 = "engine_getPayloadV2"
	// GetPayloadMethodV3 v3 request string for JSON-RPC.
	GetPayloadMethodV3 = "engine_getPayloadV3"
)

var (
	defaultBuilderHost = "127.0.0.1"
	defaultBuilderPort = 8551
)

type jsonRPCObject struct {
	Jsonrpc string        `json:"jsonrpc"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
	ID      uint64        `json:"id"`
	Result  interface{}   `json:"result"`
}

type ForkchoiceUpdatedResponse struct {
	Jsonrpc string        `json:"jsonrpc"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
	ID      uint64        `json:"id"`
	Result  struct {
		Status    *v1.PayloadStatus  `json:"payloadStatus"`
		PayloadId *v1.PayloadIDBytes `json:"payloadId"`
	} `json:"result"`
}

type ExecPayloadResponse struct {
	Version string               `json:"version"`
	Data    *v1.ExecutionPayload `json:"data"`
}

type ExecHeaderResponseCapella struct {
	Version string `json:"version"`
	Data    struct {
		Signature hexutil.Bytes                 `json:"signature"`
		Message   *builderAPI.BuilderBidCapella `json:"message"`
	} `json:"data"`
}

type ExecHeaderResponseDeneb struct {
	Version string `json:"version"`
	Data    struct {
		Signature hexutil.Bytes               `json:"signature"`
		Message   *builderAPI.BuilderBidDeneb `json:"message"`
	} `json:"data"`
}

type Builder struct {
	cfg            *config
	address        string
	execClient     *gethRPC.Client
	currId         *v1.PayloadIDBytes
	prevBeaconRoot []byte
	currPayload    interfaces.ExecutionData
	blobBundle     *v1.BlobsBundle
	mux            *http.ServeMux
	validatorMap   map[string]*eth.ValidatorRegistrationV1
	valLock        sync.RWMutex
	srv            *http.Server
}

// New creates a proxy server forwarding requests from a consensus client to an execution client.
func New(opts ...Option) (*Builder, error) {
	p := &Builder{
		cfg: &config{
			builderPort: defaultBuilderPort,
			builderHost: defaultBuilderHost,
			logger:      logrus.New(),
		},
	}
	for _, o := range opts {
		if err := o(p); err != nil {
			return nil, err
		}
	}
	if p.cfg.destinationUrl == nil {
		return nil, errors.New("must provide a destination address for request proxying")
	}
	endpoint := network.HttpEndpoint(p.cfg.destinationUrl.String())
	endpoint.Auth.Method = authorization.Bearer
	endpoint.Auth.Value = p.cfg.secret
	execClient, err := network.NewExecutionRPCClient(context.Background(), endpoint, nil)
	if err != nil {
		return nil, err
	}
	router := http.NewServeMux()
	router.Handle("/", p)
	router.HandleFunc(statusPath, func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusOK)
	})
	router.HandleFunc(registerPath, p.registerValidators)
	router.HandleFunc(headerPath, p.handleHeaderRequest)
	router.HandleFunc(blindedPath, p.handleBlindedBlock)
	addr := net.JoinHostPort(p.cfg.builderHost, strconv.Itoa(p.cfg.builderPort))
	srv := &http.Server{
		Handler:           router,
		Addr:              addr,
		ReadHeaderTimeout: time.Second,
	}
	p.address = addr
	p.srv = srv
	p.execClient = execClient
	p.valLock.Lock()
	p.validatorMap = map[string]*eth.ValidatorRegistrationV1{}
	p.valLock.Unlock()
	p.mux = router
	return p, nil
}

// Address for the proxy server.
func (p *Builder) Address() string {
	return p.address
}

// Start a proxy server.
func (p *Builder) Start(ctx context.Context) error {
	p.srv.BaseContext = func(listener net.Listener) context.Context {
		return ctx
	}
	p.cfg.logger.WithFields(logrus.Fields{
		"executionAddress": p.cfg.destinationUrl.String(),
	}).Infof("Builder now listening on address %s", p.address)
	go func() {
		if err := p.srv.ListenAndServe(); err != nil {
			p.cfg.logger.Error(err)
		}
	}()
	for {
		<-ctx.Done()
		return p.srv.Shutdown(context.Background())
	}
}

// ServeHTTP requests from a consensus client to an execution client, modifying in-flight requests
// and/or responses as desired. It also processes any backed-up requests.
func (p *Builder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.cfg.logger.Infof("Received %s request from beacon with url: %s", r.Method, r.URL.Path)
	if p.isBuilderCall(r) {
		p.mux.ServeHTTP(w, r)
		return
	}
	requestBytes, err := parseRequestBytes(r)
	if err != nil {
		p.cfg.logger.WithError(err).Error("Could not parse request")
		return
	}
	execRes, err := p.sendHttpRequest(r, requestBytes)
	if err != nil {
		p.cfg.logger.WithError(err).Error("Could not forward request")
		return
	}
	p.cfg.logger.Infof("Received response for %s request with method %s from %s", r.Method, r.Method, p.cfg.destinationUrl.String())

	defer func() {
		if err = execRes.Body.Close(); err != nil {
			p.cfg.logger.WithError(err).Error("Could not do close proxy responseGen body")
		}
	}()

	buf := bytes.NewBuffer([]byte{})
	if _, err = io.Copy(buf, execRes.Body); err != nil {
		p.cfg.logger.WithError(err).Error("Could not copy proxy request body")
		return
	}
	byteResp := bytesutil.SafeCopyBytes(buf.Bytes())
	p.handleEngineCalls(requestBytes, byteResp)
	// Pipe the proxy responseGen to the original caller.
	if _, err = io.Copy(w, buf); err != nil {
		p.cfg.logger.WithError(err).Error("Could not copy proxy request body")
		return
	}
}

func (p *Builder) handleEngineCalls(req, resp []byte) {
	if !isEngineAPICall(req) {
		return
	}
	rpcObj, err := unmarshalRPCObject(req)
	if err != nil {
		p.cfg.logger.WithError(err).Error("Could not unmarshal rpc object")
		return
	}
	p.cfg.logger.Infof("Received engine call %s", rpcObj.Method)
	switch rpcObj.Method {
	case ForkchoiceUpdatedMethod, ForkchoiceUpdatedMethodV2, ForkchoiceUpdatedMethodV3:
		result := &ForkchoiceUpdatedResponse{}
		err = json.Unmarshal(resp, result)
		if err != nil {
			p.cfg.logger.Errorf("Could not unmarshal fcu: %v", err)
			return
		}
		if result.Result.PayloadId != nil && *result.Result.PayloadId != [8]byte{} {
			p.currId = result.Result.PayloadId
		}
		if rpcObj.Method == ForkchoiceUpdatedMethodV3 {
			attr := &v1.PayloadAttributesV3{}
			obj, err := json.Marshal(rpcObj.Params[1])
			if err != nil {
				p.cfg.logger.Errorf("Could not marshal attr: %v", err)
				return
			}
			if err := json.Unmarshal(obj, attr); err != nil {
				p.cfg.logger.Errorf("Could not unmarshal attr: %v", err)
				return
			}
			p.prevBeaconRoot = attr.ParentBeaconBlockRoot
		}
		payloadID := [8]byte{}
		status := ""
		var lastValHash []byte
		if result.Result.PayloadId != nil {
			payloadID = *result.Result.PayloadId
		}
		if result.Result.Status != nil {
			status = result.Result.Status.Status.String()
			lastValHash = result.Result.Status.LatestValidHash
		}
		p.cfg.logger.Infof("Received payload id of %#x and status of %s along with a valid hash of %#x", payloadID, status, lastValHash)
	}
}

func (*Builder) isBuilderCall(req *http.Request) bool {
	return strings.Contains(req.URL.Path, "/eth/v1/builder/")
}

func (p *Builder) registerValidators(w http.ResponseWriter, req *http.Request) {
	var registrations []structs.SignedValidatorRegistration
	if err := json.NewDecoder(req.Body).Decode(&registrations); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	for _, r := range registrations {
		msg, err := r.Message.ToConsensus()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		p.valLock.Lock()
		p.validatorMap[r.Message.Pubkey] = msg
		p.valLock.Unlock()
	}
	// TODO: Verify Signatures from validators
	w.WriteHeader(http.StatusOK)
}

func (p *Builder) handleHeaderRequest(w http.ResponseWriter, req *http.Request) {
	pHash := req.PathValue("parent_hash")
	if pHash == "" {
		http.Error(w, "no valid parent hash", http.StatusBadRequest)
		return
	}
	_, err := bytesutil.DecodeHexWithLength(pHash, common.HashLength)
	if err != nil {
		http.Error(w, "invalid parent hash", http.StatusBadRequest)
		return
	}
	reqSlot := req.PathValue("slot")
	if reqSlot == "" {
		http.Error(w, "no valid slot provided", http.StatusBadRequest)
		return
	}
	slot, err := strconv.Atoi(reqSlot)
	if err != nil {
		http.Error(w, "invalid slot provided", http.StatusBadRequest)
		return
	}
	reqPubkey := req.PathValue("pubkey")
	if reqPubkey == "" {
		http.Error(w, "no valid pubkey provided", http.StatusBadRequest)
		return
	}
	_, err = bytesutil.DecodeHexWithLength(reqPubkey, fieldparams.BLSPubkeyLength)
	if err != nil {
		http.Error(w, "invalid pubkey", http.StatusBadRequest)
		return
	}
	ax := types.Slot(slot)
	currEpoch := types.Epoch(ax / params.BeaconConfig().SlotsPerEpoch)
	if currEpoch >= params.BeaconConfig().DenebForkEpoch {
		p.handleHeaderRequestDeneb(w)
		return
	}

	if currEpoch >= params.BeaconConfig().CapellaForkEpoch {
		p.handleHeaderRequestCapella(w)
		return
	}

	b, err := p.retrievePendingBlock()
	if err != nil {
		p.cfg.logger.WithError(err).Error("Could not retrieve pending block")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	secKey, err := bls.RandKey()
	if err != nil {
		p.cfg.logger.WithError(err).Error("Could not retrieve secret key")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	wObj, err := blocks.WrappedExecutionPayload(b)
	if err != nil {
		p.cfg.logger.WithError(err).Error("Could not wrap execution payload")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	hdr, err := blocks.PayloadToHeader(wObj)
	if err != nil {
		p.cfg.logger.WithError(err).Error("Could not make payload into header")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	gEth := big.NewInt(int64(params.BeaconConfig().GweiPerEth))
	weiEth := gEth.Mul(gEth, gEth)
	val := builderAPI.Uint256{Int: weiEth}
	wrappedHdr := &builderAPI.ExecutionPayloadHeader{ExecutionPayloadHeader: hdr}
	bid := &builderAPI.BuilderBid{
		Header: wrappedHdr,
		Value:  val,
		Pubkey: secKey.PublicKey().Marshal(),
	}
	sszBid := &eth.BuilderBid{
		Header: hdr,
		Value:  val.SSZBytes(),
		Pubkey: secKey.PublicKey().Marshal(),
	}
	d, err := signing.ComputeDomain(params.BeaconConfig().DomainApplicationBuilder,
		nil, /* fork version */
		nil /* genesis val root */)
	if err != nil {
		p.cfg.logger.WithError(err).Error("Could not compute the domain")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rt, err := signing.ComputeSigningRoot(sszBid, d)
	if err != nil {
		p.cfg.logger.WithError(err).Error("Could not compute the signing root")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sig := secKey.Sign(rt[:])
	hdrResp := &builderAPI.ExecHeaderResponse{
		Version: "bellatrix",
		Data: struct {
			Signature hexutil.Bytes          `json:"signature"`
			Message   *builderAPI.BuilderBid `json:"message"`
		}{
			Signature: sig.Marshal(),
			Message:   bid,
		},
	}

	err = json.NewEncoder(w).Encode(hdrResp)
	if err != nil {
		p.cfg.logger.WithError(err).Error("Could not encode response")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p.currPayload = wObj
	w.WriteHeader(http.StatusOK)
}

func (p *Builder) handleHeaderRequestCapella(w http.ResponseWriter) {
	b, err := p.retrievePendingBlockCapella()
	if err != nil {
		p.cfg.logger.WithError(err).Error("Could not retrieve pending block")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	secKey, err := bls.RandKey()
	if err != nil {
		p.cfg.logger.WithError(err).Error("Could not retrieve secret key")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	v := big.NewInt(0).SetBytes(bytesutil.ReverseByteOrder(b.Value))
	// we set the payload value as twice its actual one so that it always chooses builder payloads vs local payloads
	v = v.Mul(v, big.NewInt(2))
	wObj, err := blocks.WrappedExecutionPayloadCapella(b.Payload)
	if err != nil {
		p.cfg.logger.WithError(err).Error("Could not wrap execution payload")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	hdr, err := blocks.PayloadToHeaderCapella(wObj)
	if err != nil {
		p.cfg.logger.WithError(err).Error("Could not make payload into header")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	val := builderAPI.Uint256{Int: v}
	wrappedHdr := &builderAPI.ExecutionPayloadHeaderCapella{ExecutionPayloadHeaderCapella: hdr}
	bid := &builderAPI.BuilderBidCapella{
		Header: wrappedHdr,
		Value:  val,
		Pubkey: secKey.PublicKey().Marshal(),
	}
	sszBid := &eth.BuilderBidCapella{
		Header: hdr,
		Value:  val.SSZBytes(),
		Pubkey: secKey.PublicKey().Marshal(),
	}
	d, err := signing.ComputeDomain(params.BeaconConfig().DomainApplicationBuilder,
		nil, /* fork version */
		nil /* genesis val root */)
	if err != nil {
		p.cfg.logger.WithError(err).Error("Could not compute the domain")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rt, err := signing.ComputeSigningRoot(sszBid, d)
	if err != nil {
		p.cfg.logger.WithError(err).Error("Could not compute the signing root")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sig := secKey.Sign(rt[:])
	hdrResp := &ExecHeaderResponseCapella{
		Version: "capella",
		Data: struct {
			Signature hexutil.Bytes                 `json:"signature"`
			Message   *builderAPI.BuilderBidCapella `json:"message"`
		}{
			Signature: sig.Marshal(),
			Message:   bid,
		},
	}

	err = json.NewEncoder(w).Encode(hdrResp)
	if err != nil {
		p.cfg.logger.WithError(err).Error("Could not encode response")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p.currPayload = wObj
	w.WriteHeader(http.StatusOK)
}

func (p *Builder) handleHeaderRequestDeneb(w http.ResponseWriter) {
	b, err := p.retrievePendingBlockDeneb()
	if err != nil {
		p.cfg.logger.WithError(err).Error("Could not retrieve pending block")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	secKey, err := bls.RandKey()
	if err != nil {
		p.cfg.logger.WithError(err).Error("Could not retrieve secret key")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	v := big.NewInt(0).SetBytes(bytesutil.ReverseByteOrder(b.Value))
	// we set the payload value as twice its actual one so that it always chooses builder payloads vs local payloads
	v = v.Mul(v, big.NewInt(2))
	wObj, err := blocks.WrappedExecutionPayloadDeneb(b.Payload)
	if err != nil {
		p.cfg.logger.WithError(err).Error("Could not wrap execution payload")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	hdr, err := blocks.PayloadToHeaderDeneb(wObj)
	if err != nil {
		p.cfg.logger.WithError(err).Error("Could not make payload into header")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	val := builderAPI.Uint256{Int: v}
	var commitments []hexutil.Bytes
	for _, c := range b.BlobsBundle.KzgCommitments {
		copiedC := c
		commitments = append(commitments, copiedC)
	}
	wrappedHdr := &builderAPI.ExecutionPayloadHeaderDeneb{ExecutionPayloadHeaderDeneb: hdr}
	bid := &builderAPI.BuilderBidDeneb{
		Header:             wrappedHdr,
		BlobKzgCommitments: commitments,
		Value:              val,
		Pubkey:             secKey.PublicKey().Marshal(),
	}
	sszBid := &eth.BuilderBidDeneb{
		Header:             hdr,
		BlobKzgCommitments: b.BlobsBundle.KzgCommitments,
		Value:              val.SSZBytes(),
		Pubkey:             secKey.PublicKey().Marshal(),
	}
	d, err := signing.ComputeDomain(params.BeaconConfig().DomainApplicationBuilder,
		nil, /* fork version */
		nil /* genesis val root */)
	if err != nil {
		p.cfg.logger.WithError(err).Error("Could not compute the domain")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rt, err := signing.ComputeSigningRoot(sszBid, d)
	if err != nil {
		p.cfg.logger.WithError(err).Error("Could not compute the signing root")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sig := secKey.Sign(rt[:])
	hdrResp := &ExecHeaderResponseDeneb{
		Version: "deneb",
		Data: struct {
			Signature hexutil.Bytes               `json:"signature"`
			Message   *builderAPI.BuilderBidDeneb `json:"message"`
		}{
			Signature: sig.Marshal(),
			Message:   bid,
		},
	}

	err = json.NewEncoder(w).Encode(hdrResp)
	if err != nil {
		p.cfg.logger.WithError(err).Error("Could not encode response")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p.currPayload = wObj
	p.blobBundle = b.BlobsBundle
	w.WriteHeader(http.StatusOK)
}

func (p *Builder) handleBlindedBlock(w http.ResponseWriter, req *http.Request) {
	sb := &builderAPI.SignedBlindedBeaconBlockBellatrix{
		SignedBlindedBeaconBlockBellatrix: &eth.SignedBlindedBeaconBlockBellatrix{},
	}
	err := json.NewDecoder(req.Body).Decode(sb)
	if err != nil {
		p.cfg.logger.WithError(err).Error("Could not decode blinded block")
		// TODO: Allow the method to unmarshal blinded blocks correctly
	}
	if p.currPayload == nil {
		p.cfg.logger.Error("No payload is cached")
		http.Error(w, "payload not found", http.StatusInternalServerError)
		return
	}

	resp, err := builderAPI.ExecutionPayloadResponseFromData(p.currPayload, p.blobBundle)
	if err != nil {
		p.cfg.logger.WithError(err).Error("Could not convert the payload")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	err = json.NewEncoder(w).Encode(resp)
	if err != nil {
		p.cfg.logger.WithError(err).Error("Could not encode full payload response")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (p *Builder) retrievePendingBlock() (*v1.ExecutionPayload, error) {
	result := &engine.ExecutableData{}
	if p.currId == nil {
		return nil, errors.New("no payload id is cached")
	}
	err := p.execClient.CallContext(context.Background(), result, GetPayloadMethod, *p.currId)
	if err != nil {
		return nil, err
	}
	payloadEnv, err := modifyExecutionPayload(*result, big.NewInt(0), nil)
	if err != nil {
		return nil, err
	}
	marshalledOutput, err := payloadEnv.ExecutionPayload.MarshalJSON()
	if err != nil {
		return nil, err
	}
	bellatrixPayload := &v1.ExecutionPayload{}
	if err = json.Unmarshal(marshalledOutput, bellatrixPayload); err != nil {
		return nil, err
	}
	p.currId = nil
	return bellatrixPayload, nil
}

func (p *Builder) retrievePendingBlockCapella() (*v1.ExecutionPayloadCapellaWithValue, error) {
	result := &engine.ExecutionPayloadEnvelope{}
	if p.currId == nil {
		return nil, errors.New("no payload id is cached")
	}
	err := p.execClient.CallContext(context.Background(), result, GetPayloadMethodV2, *p.currId)
	if err != nil {
		return nil, err
	}
	payloadEnv, err := modifyExecutionPayload(*result.ExecutionPayload, result.BlockValue, nil)
	if err != nil {
		return nil, err
	}
	marshalledOutput, err := payloadEnv.MarshalJSON()
	if err != nil {
		return nil, err
	}
	capellaPayload := &v1.ExecutionPayloadCapellaWithValue{}
	if err = json.Unmarshal(marshalledOutput, capellaPayload); err != nil {
		return nil, err
	}
	p.currId = nil
	return capellaPayload, nil
}

func (p *Builder) retrievePendingBlockDeneb() (*v1.ExecutionPayloadDenebWithValueAndBlobsBundle, error) {
	result := &engine.ExecutionPayloadEnvelope{}
	if p.currId == nil {
		return nil, errors.New("no payload id is cached")
	}
	err := p.execClient.CallContext(context.Background(), result, GetPayloadMethodV3, *p.currId)
	if err != nil {
		return nil, err
	}
	if p.prevBeaconRoot == nil {
		p.cfg.logger.Errorf("previous root is nil")
	}
	payloadEnv, err := modifyExecutionPayload(*result.ExecutionPayload, result.BlockValue, p.prevBeaconRoot)
	if err != nil {
		return nil, err
	}
	payloadEnv.BlobsBundle = result.BlobsBundle
	marshalledOutput, err := payloadEnv.MarshalJSON()
	if err != nil {
		return nil, err
	}
	denebPayload := &v1.ExecutionPayloadDenebWithValueAndBlobsBundle{}
	if err = json.Unmarshal(marshalledOutput, denebPayload); err != nil {
		return nil, err
	}
	p.currId = nil
	return denebPayload, nil
}

func (p *Builder) sendHttpRequest(req *http.Request, requestBytes []byte) (*http.Response, error) {
	proxyReq, err := http.NewRequest(req.Method, p.cfg.destinationUrl.String(), req.Body)
	if err != nil {
		p.cfg.logger.WithError(err).Error("Could not create new request")
		return nil, err
	}

	// Set the modified request as the proxy request body.
	proxyReq.Body = io.NopCloser(bytes.NewBuffer(requestBytes))

	// Required proxy headers for forwarding JSON-RPC requests to the execution client.
	proxyReq.Header.Set("Host", req.Host)
	proxyReq.Header.Set("X-Forwarded-For", req.RemoteAddr)
	proxyReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	if p.cfg.secret != "" {
		client = network.NewHttpClientWithSecret(p.cfg.secret, "")
	}
	proxyRes, err := client.Do(proxyReq)
	if err != nil {
		p.cfg.logger.WithError(err).Error("Could not forward request to destination server")
		return nil, err
	}
	return proxyRes, nil
}

// Peek into the bytes of an HTTP request's body.
func parseRequestBytes(req *http.Request) ([]byte, error) {
	requestBytes, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	if err = req.Body.Close(); err != nil {
		return nil, err
	}
	req.Body = io.NopCloser(bytes.NewBuffer(requestBytes))
	return requestBytes, nil
}

// Checks whether the JSON-RPC request is for the Ethereum engine API.
func isEngineAPICall(reqBytes []byte) bool {
	jsonRequest, err := unmarshalRPCObject(reqBytes)
	if err != nil {
		switch {
		case strings.Contains(err.Error(), "cannot unmarshal array"):
			return false
		default:
			return false
		}
	}
	return strings.Contains(jsonRequest.Method, "engine_")
}

func unmarshalRPCObject(b []byte) (*jsonRPCObject, error) {
	r := &jsonRPCObject{}
	if err := json.Unmarshal(b, r); err != nil {
		return nil, err
	}
	return r, nil
}

func modifyExecutionPayload(execPayload engine.ExecutableData, fees *big.Int, prevBeaconRoot []byte) (*engine.ExecutionPayloadEnvelope, error) {
	modifiedBlock, err := executableDataToBlock(execPayload, prevBeaconRoot)
	if err != nil {
		return &engine.ExecutionPayloadEnvelope{}, err
	}
	return engine.BlockToExecutableData(modifiedBlock, fees, nil /*blobs*/), nil
}

// This modifies the provided payload to imprint the builder's extra data
func executableDataToBlock(params engine.ExecutableData, prevBeaconRoot []byte) (*gethTypes.Block, error) {
	txs, err := decodeTransactions(params.Transactions)
	if err != nil {
		return nil, err
	}
	// Only set withdrawalsRoot if it is non-nil. This allows CLs to use
	// ExecutableData before withdrawals are enabled by marshaling
	// Withdrawals as the json null value.
	var withdrawalsRoot *common.Hash
	if params.Withdrawals != nil {
		h := gethTypes.DeriveSha(gethTypes.Withdrawals(params.Withdrawals), trie.NewStackTrie(nil))
		withdrawalsRoot = &h
	}
	header := &gethTypes.Header{
		ParentHash:      params.ParentHash,
		UncleHash:       gethTypes.EmptyUncleHash,
		Coinbase:        params.FeeRecipient,
		Root:            params.StateRoot,
		TxHash:          gethTypes.DeriveSha(gethTypes.Transactions(txs), trie.NewStackTrie(nil)),
		ReceiptHash:     params.ReceiptsRoot,
		Bloom:           gethTypes.BytesToBloom(params.LogsBloom),
		Difficulty:      common.Big0,
		Number:          new(big.Int).SetUint64(params.Number),
		GasLimit:        params.GasLimit,
		GasUsed:         params.GasUsed,
		Time:            params.Timestamp,
		BaseFee:         params.BaseFeePerGas,
		Extra:           []byte("prysm-builder"), // add in extra data
		MixDigest:       params.Random,
		WithdrawalsHash: withdrawalsRoot,
		BlobGasUsed:     params.BlobGasUsed,
		ExcessBlobGas:   params.ExcessBlobGas,
	}
	if prevBeaconRoot != nil {
		pRoot := common.Hash(prevBeaconRoot)
		header.ParentBeaconRoot = &pRoot
	}
	block := gethTypes.NewBlockWithHeader(header).WithBody(txs, nil /* uncles */).WithWithdrawals(params.Withdrawals)
	return block, nil
}

func decodeTransactions(enc [][]byte) ([]*gethTypes.Transaction, error) {
	var txs = make([]*gethTypes.Transaction, len(enc))
	for i, encTx := range enc {
		var tx gethTypes.Transaction
		if err := tx.UnmarshalBinary(encTx); err != nil {
			return nil, fmt.Errorf("invalid transaction %d: %w", i, err)
		}
		txs[i] = &tx
	}
	return txs, nil
}