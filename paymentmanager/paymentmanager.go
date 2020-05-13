package paymentmanager

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"sync"
	"time"

	logging "github.com/ipfs/go-log"
	"github.com/ipfs/go-metrics-interface"

	bsnet "github.com/ipfs/go-bitswap/network"
	"github.com/libp2p/go-libp2p-core/peer"

	"github.com/gorilla/mux"
	"net/http"
)

var log = logging.Logger("bitswap")

type CommandModel struct {
	CommandId	string
	CommandType int32
	CommandBody string
	NodeId		string
}

type CommandResponseModel struct {
	CommandResponse	string
	CommandId		string
	NodeId			string
}

// PeerHandler sends changes out to the network as they get added to the payment list
type PeerHandler interface {
	InitiatePayment(target peer.ID, paymentRequest string)

	PaymentCommand(target peer.ID, commandId string, commandBody string, commandType int32)

	PaymentResponse(target peer.ID, commandId string, commandReply string)
}

type PaymentHandler interface {
	GetDebt(id peer.ID) *Debt
	CallProcessCommand(commandId string, commandType int32, commandBody string, nodeId string) error
	CallProcessPayment(paymentRequest string, requestReference string)
	CreatePaymentInfo(amount int) (string, error)
	CallProcessResponse(commandId string, responseBody string, nodeId string)
}

type paymentMessage interface {
	handle(paymentHandler PaymentHandler, peerHandler PeerHandler)
}

// Payment manager manages payment requests and process actual payments over the Stellar network
type PaymentManager struct {
	paymentMessages 	chan paymentMessage

	ctx    				context.Context
	cancel				func()

	network      		bsnet.BitSwapNetwork
	peerHandler  		PeerHandler
	paymentGauge 		metrics.Gauge

	debtRegistry		map[peer.ID]*Debt

	commandListenPort	int
	channelUrl			string

	server				*http.Server
}

type Debt struct {
	id peer.ID

	validationQueue		*list.List

	requestedAmount		int

	transferredBytes 	int

	receivedBytes 		int
}

const (
	requestPaymentAfterBytes = 50 * 1024 * 1024 // Pey per each 50 MB including transaction fee => 50 * 0.00002 + 0.00001 = 0.00101 XLM , 1 XLM pays for 49,5GB of data
)

// New initializes a new WantManager for a given context.
func New(ctx context.Context, peerHandler PeerHandler, network bsnet.BitSwapNetwork) *PaymentManager {
	ctx, cancel := context.WithCancel(ctx)
	paymentGauge := metrics.NewCtx(ctx, "payments_total",
		"Number of items in payments queue.").Gauge()

	registry := make(map[peer.ID]*Debt)

	return &PaymentManager{
		paymentMessages:  make(chan paymentMessage, 10),
		ctx:           	ctx,
		cancel:        	cancel,
		peerHandler:   	peerHandler,
		paymentGauge: 	paymentGauge,
		network:		network,
		debtRegistry:	registry,
	}
}

func (pm *PaymentManager) SetPPChannelSettings(commandListenPort int, channelUrl string) {
	pm.commandListenPort = commandListenPort
	pm.channelUrl		 = channelUrl
}

// Startup starts processing for the PayManager.
func (pm *PaymentManager) Startup() {
	go pm.run()

	go pm.validationTimer()

	go pm.startCommandServer()
}

// Shutdown ends processing for the pay manager.
func (pm *PaymentManager) Shutdown() {
	pm.cancel()

	pm.server.Shutdown(pm.ctx)
}

func (pm *PaymentManager) startCommandServer() {
	router := mux.NewRouter()

	router.HandleFunc("/api/command", pm.ProcessCommand).Methods("POST")
	router.HandleFunc("/api/commandResponse", pm.ProcessCommandResponse).Methods("POST")

	pm.server = &http.Server{
		Addr: fmt.Sprintf(":%d",pm.commandListenPort),
		Handler: router,
	}

	err := pm.server.ListenAndServe()

	if err != nil {
		panic(err)
	}
}

func (pm *PaymentManager) run() {
	// NOTE: Do not open any streams or connections from anywhere in this
	// event loop. Really, just don't do anything likely to block.
	for {
		select {
		case message := <-pm.paymentMessages:
			message.handle(pm, pm.peerHandler)
		case <-pm.ctx.Done():
			return
		}
	}
}

func (pm *PaymentManager) validationTimer() {
	for {
		timer := time.NewTimer(5 * time.Second)

		select {
		case <- timer.C:
			pm.validatePeers()
			timer.Reset(5 * time.Second) // Restart timer after validation finished
		case <-pm.ctx.Done():
			timer.Stop()
			return
		}
	}
}

func (pm *PaymentManager) validatePeers() {
	var wg sync.WaitGroup

	for _, debt := range pm.debtRegistry {
		wg.Add(1)

		go func() {
			defer wg.Done()

			debt.validateTransactions(pm)
		}()
	}

	wg.Wait()
}

func (d *Debt) validateTransactions(pm PaymentHandler) {

}

func (pm *PaymentManager) GetDebt(id peer.ID) *Debt {
	debt, ok := pm.debtRegistry[id]

	if ok {
		return debt
	}

	debt = &Debt {
		id: id,
		validationQueue: list.New(),
	}

	pm.debtRegistry[id] = debt

	return debt
}

func (pm *PaymentManager) ProcessCommandResponse(w http.ResponseWriter, r *http.Request) {
	// Extract command response from the request and forward it to peer
	request := &CommandResponseModel{}

	err := json.NewDecoder(r.Body).Decode(request)

	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, err.Error())

		return
	}

	targetId, err := peer.IDHexDecode(request.NodeId)

	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, err.Error())

		return
	}

	select {
	case pm.paymentMessages <- &processIncomingPaymentCommandResponse{
		target:      targetId,
		commandId:   request.CommandId,
		commandResponse: request.CommandResponse,
	}:
		w.WriteHeader(http.StatusOK)
	case <-pm.ctx.Done():
		w.WriteHeader(http.StatusRequestTimeout)
	}
}

func (pm *PaymentManager) ProcessCommand(w http.ResponseWriter, r *http.Request) {
	// Extract command request from the request and forward it to peer
	request := &CommandModel{}

	err := json.NewDecoder(r.Body).Decode(request)

	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, err.Error())

		return
	}

	targetId, err := peer.IDHexDecode(request.NodeId)

	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, err.Error())

		return
	}

	select {
	case pm.paymentMessages <- &processIncomingPaymentCommand{
		target:      targetId,
		commandId:   request.CommandId,
		commandType: request.CommandType,
		commandBody: request.CommandBody,
	}:
		w.WriteHeader(http.StatusOK)
	case <-pm.ctx.Done():
		w.WriteHeader(http.StatusRequestTimeout)
	}
}

// Process payment request received from {id} peer
func (pm *PaymentManager) ProcessPaymentRequest(ctx context.Context, id peer.ID, paymentRequest string) {
	select {
	case pm.paymentMessages <- &initiatePayment{from: id, paymentRequest: paymentRequest}:
	case <-pm.ctx.Done():
	case <-ctx.Done():
	}
}

// Register {msgSize} bytes sent to {id} peer and initiate payment request
func (pm *PaymentManager) RequirePayment(ctx context.Context, id peer.ID, msgSize int) {
	select {
	case pm.paymentMessages <- &requirePayment{target: id, msgSize: msgSize}:
	case <-pm.ctx.Done():
	case <-ctx.Done():
	}
}

// Register {msgSize} bytes received from {id} peer
func (pm *PaymentManager) RegisterReceivedBytes(ctx context.Context, id peer.ID, msgSize int) {
	select {
	case pm.paymentMessages <- &registerReceivedBytes{from: id, msgSize: msgSize}:
	case <-pm.ctx.Done():
	case <-ctx.Done():
	}
}

// Process payment command received from {id} peer
func (pm *PaymentManager) ProcessPaymentCommand(ctx context.Context, id peer.ID, commandId string, commandBody string, commandType int32) {
	select {
	case pm.paymentMessages <- &processOutgoingPaymentCommand{from: id, commandId: commandId, commandBody: commandBody, commandType: commandType}:
	case <-pm.ctx.Done():
	case <-ctx.Done():
	}
}

// Process payment response received from {id} peer
func (pm *PaymentManager) ProcessPaymentResponse(ctx context.Context, id peer.ID, commandId string, commandReply string) {
	select {
	case pm.paymentMessages <- &processPaymentResponse{from: id, commandId: commandId, commandReply: commandReply}:
	case <-pm.ctx.Done():
	case <-ctx.Done():
	}
}

type initiatePayment struct {
	from peer.ID
	paymentRequest string
}

func (i initiatePayment) handle(pm PaymentHandler, peerHandler PeerHandler)  {
	pm.CallProcessPayment(i.paymentRequest, peer.IDHexEncode(i.from))
}

type registerReceivedBytes struct {
	from peer.ID
	msgSize int
}

func (r registerReceivedBytes) handle(handler PaymentHandler, peerHandler PeerHandler)  {
	debt := handler.GetDebt(r.from)

	debt.receivedBytes += r.msgSize
}

type requirePayment struct {
	target peer.ID
	msgSize int
}

func (r requirePayment) handle(handler PaymentHandler, peerHandler PeerHandler) {
	debt := handler.GetDebt(r.target)

	debt.transferredBytes += r.msgSize

	if debt.transferredBytes >= requestPaymentAfterBytes {
		amount := debt.transferredBytes

		paymentRequest, err := handler.CreatePaymentInfo(amount)

		if err != nil {
			log.Error("create payment info failed: %s", err.Error())

			return
		}

		peerHandler.InitiatePayment(r.target, paymentRequest)

		debt.requestedAmount += amount
		debt.transferredBytes = 0
	}
}

type processIncomingPaymentCommandResponse struct {
	target 			peer.ID
	commandId		string
	commandResponse string
}

func (p processIncomingPaymentCommandResponse) handle(pm PaymentHandler, peerHandler PeerHandler) {

	peerHandler.PaymentResponse(p.target, p.commandId, p.commandResponse)
}

type processIncomingPaymentCommand struct {
	target 		peer.ID
	commandId	string
	commandType int32
	commandBody string
}

func (p processIncomingPaymentCommand) handle(pm PaymentHandler, peerHandler PeerHandler) {
	peerHandler.PaymentCommand(p.target, p.commandId, p.commandBody, p.commandType)
}

type processPaymentResponse struct {
	from 			peer.ID
	commandId		string
	commandReply	string
}

func (v processPaymentResponse) handle(handler PaymentHandler, peerHandler PeerHandler) {
	handler.CallProcessResponse(v.commandId, v.commandReply, peer.IDHexEncode(v.from))
}

type processOutgoingPaymentCommand struct {
	from 		peer.ID
	commandId	string
	commandBody	string
	commandType	int32
}

func (p processOutgoingPaymentCommand) handle(pm PaymentHandler, peerHandler PeerHandler) {
	err := pm.CallProcessCommand(p.commandId, p.commandType, p.commandBody, peer.IDHexEncode(p.from))

	if err != nil {
		log.Error("process command failed: %s", err.Error())
	}
}

func (pm *PaymentManager) CallProcessResponse(commandId string, responseBody string, nodeId string) {
	values := map[string]string {
		"CommandId":   	commandId,
		"ResponseBody": responseBody,
		"NodeId": 		nodeId,
	}

	jsonValue, err := json.Marshal(values)

	if err != nil {
		log.Error("failed to serialize process command response: %s", err.Error())

		return
	}

	reply, err := http.Post(fmt.Sprintf("%s/api/gateway/processResponse", pm.channelUrl), "application/json", bytes.NewBuffer(jsonValue))

	if err != nil {
		log.Error("failed to call process command response: %s", err.Error())

		return
	}

	log.Info("process command response call status: %d", reply.StatusCode)
}


func (pm *PaymentManager) CallProcessCommand(commandId string, commandType int32, commandBody string, nodeId string) error {
	values := map[string]interface{} {
		"NodeId":		nodeId,
		"CommandId":	commandId,
		"CommandType":	commandType,
		"CommandBody":	commandBody,
		"CallbackUrl":	fmt.Sprintf("http://localhost:%d/api/commandResponse", pm.commandListenPort),
	}

	jsonValue, err := json.Marshal(values)

	if err != nil {
		log.Error("failed to serialize process command request: %s", err.Error())

		return err
	}

	reply, err := http.Post(fmt.Sprintf("%s/api/utility/processCommand", pm.channelUrl), "application/json", bytes.NewBuffer(jsonValue))

	if err != nil {
		log.Error("failed to call process command: %s", err.Error())

		return err
	}

	defer reply.Body.Close()

	log.Info("process command call status: %d", reply.StatusCode)

	return nil
}

func (pm *PaymentManager) CallProcessPayment(paymentRequest string, nodeId string) {
	values := map[string]string {
		"CallbackUrl":      fmt.Sprintf("http://localhost:%d/api/command", pm.commandListenPort),
		"PaymentRequest":   paymentRequest,
		"NodeId": 			nodeId,
	}

	jsonValue, err := json.Marshal(values)

	if err != nil {
		log.Error("failed to serialize process payment request: %s", err.Error())

		return
	}

	reply, err := http.Post(fmt.Sprintf("%s/api/gateway/processPayment", pm.channelUrl), "application/json", bytes.NewBuffer(jsonValue))

	if err != nil {
		log.Error("failed to call process payment: %s", err.Error())

		return
	}

	log.Info("process payment call status: %d", reply.StatusCode)
}

func (pm *PaymentManager) CreatePaymentInfo(amount int) (string, error) {
	values := map[string]interface{}{"ServiceType": "ipfs", "CommodityType": "data", "Amount": amount}

	jsonValue, err := json.Marshal(values)

	if err != nil {
		log.Error("failed to serialize create payment request: %s", err.Error())

		return "", err
	}

	reply, err := http.Post(fmt.Sprintf("%s/api/utility/createPaymentInfo", pm.channelUrl), "application/json", bytes.NewBuffer(jsonValue))

	if err != nil {
		log.Error("failed to call create payment: %s", err.Error())

		return "", err
	}

	defer reply.Body.Close()

	log.Info("create payment call status: %d", reply.StatusCode)

	bodyBytes, err := ioutil.ReadAll(reply.Body)

	if err != nil {
		log.Error("failed to read create payment response body: %s", err.Error())

		return "", err
	}

	return string(bodyBytes), nil
}
