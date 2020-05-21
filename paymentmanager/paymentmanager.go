package paymentmanager

import (
	"container/list"
	"context"
	"sync"
	"time"

	logging "github.com/ipfs/go-log"
	"github.com/ipfs/go-metrics-interface"

	bsnet "github.com/ipfs/go-bitswap/network"
	"github.com/libp2p/go-libp2p-core/peer"
)

var log = logging.Logger("bitswap")

type CommandModel struct {
	CommandId	string
	CommandType int32
	CommandBody []byte
	NodeId		string
}

type CommandResponseModel struct {
	CommandResponse	[]byte
	CommandId		string
	NodeId			string
}

// PeerHandler sends changes out to the network as they get added to the payment list
type PeerHandler interface {
	InitiatePayment(target peer.ID, paymentRequest string)

	PaymentCommand(target peer.ID, commandId string, commandBody []byte, commandType int32)

	PaymentResponse(target peer.ID, commandId string, commandReply []byte)
}

type PaymentHandler interface {
	GetDebt(id peer.ID) *Debt
}

type paymentMessage interface {
	handle(paymentHandler PaymentHandler, peerHandler PeerHandler, client ClientHandler)
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

	server				CallbackHandler
	client 				ClientHandler
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
	pm.server = NewServer(commandListenPort, pm.peerHandler)
	pm.client = NewClient(channelUrl, commandListenPort)
}

// Startup starts processing for the PayManager.
func (pm *PaymentManager) Startup() {
	go pm.run()

	go pm.validationTimer()

	if pm.server != nil {
		go pm.server.Start()
	}
}

// Shutdown ends processing for the pay manager.
func (pm *PaymentManager) Shutdown() {
	pm.cancel()

	pm.server.Shutdown(pm.ctx)
}

func (pm *PaymentManager) run() {
	// NOTE: Do not open any streams or connections from anywhere in this
	// event loop. Really, just don't do anything likely to block.
	for {
		select {
		case message := <-pm.paymentMessages:
			message.handle(pm, pm.peerHandler, pm.client)
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
func (pm *PaymentManager) ProcessPaymentCommand(ctx context.Context, id peer.ID, commandId string, commandBody []byte, commandType int32) {
	select {
	case pm.paymentMessages <- &processOutgoingPaymentCommand{from: id, commandId: commandId, commandBody: commandBody, commandType: commandType}:
	case <-pm.ctx.Done():
	case <-ctx.Done():
	}
}

// Process payment response received from {id} peer
func (pm *PaymentManager) ProcessPaymentResponse(ctx context.Context, id peer.ID, commandId string, commandReply []byte) {
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

func (i initiatePayment) handle(paymentHandler PaymentHandler, peerHandler PeerHandler, client ClientHandler) {
	quantity, err := client.ValidatePayment(i.paymentRequest)

	if err != nil {
		log.Error("payment validation failed: %s", err.Error())
	}

	debt := paymentHandler.GetDebt(i.from)

	if int(quantity) < debt.receivedBytes {
		log.Error("invalid quantity requested")
	}

	client.ProcessPayment(i.paymentRequest, peer.IDHexEncode(i.from))
}

type registerReceivedBytes struct {
	from peer.ID
	msgSize int
}

func (r registerReceivedBytes) handle(paymentHandler PaymentHandler, peerHandler PeerHandler, client ClientHandler) {
	debt := paymentHandler.GetDebt(r.from)

	debt.receivedBytes += r.msgSize
}

type requirePayment struct {
	target peer.ID
	msgSize int
}

func (r requirePayment) handle(paymentHandler PaymentHandler, peerHandler PeerHandler, client ClientHandler) {
	debt := paymentHandler.GetDebt(r.target)

	debt.transferredBytes += r.msgSize

	if debt.transferredBytes >= requestPaymentAfterBytes {
		amount := debt.transferredBytes

		paymentRequest, err := client.CreatePaymentInfo(amount)

		if err != nil {
			log.Error("create payment info failed: %s", err.Error())

			return
		}

		peerHandler.InitiatePayment(r.target, paymentRequest)

		debt.requestedAmount += amount
		debt.transferredBytes = 0
	}
}

type processPaymentResponse struct {
	from 			peer.ID
	commandId		string
	commandReply	[]byte
}

func (v processPaymentResponse) handle(paymentHandler PaymentHandler, peerHandler PeerHandler, client ClientHandler) {
	client.ProcessResponse(v.commandId, v.commandReply, peer.IDHexEncode(v.from))
}

type processOutgoingPaymentCommand struct {
	from 		peer.ID
	commandId	string
	commandBody	[]byte
	commandType	int32
}

func (p processOutgoingPaymentCommand) handle(paymentHandler PaymentHandler, peerHandler PeerHandler, client ClientHandler) {
	err := client.ProcessCommand(p.commandId, p.commandType, p.commandBody, peer.IDHexEncode(p.from))

	if err != nil {
		log.Error("process command failed: %s", err.Error())
	}
}
