package paymentmanager

import (
	"container/list"
	"context"
	"strconv"
	"sync"
	"time"

	logging "github.com/ipfs/go-log"
	"github.com/ipfs/go-metrics-interface"

	bsnet "github.com/ipfs/go-bitswap/network"
	"github.com/libp2p/go-libp2p-core/peer"

	"github.com/stellar/go/clients/horizonclient"
	"github.com/stellar/go/keypair"
	"github.com/stellar/go/network"
	"github.com/stellar/go/protocols/horizon/operations"
	"github.com/stellar/go/txnbuild"
)

var log = logging.Logger("bitswap")

// PeerHandler sends changes out to the network as they get added to the payment list
type PeerHandler interface {
	SendPaymentMessage(target peer.ID, paymentHash string)
	RequirePaymentMessage(target peer.ID, amount float64)
}

type PaymentHandler interface {
	getPeerStellarKey(id peer.ID) (string, error)
	getDebt(id peer.ID) *Debt
	getOwnStellarKey() *keypair.Full
	getStellarClient()	*horizonclient.Client
}

type paymentMessage interface {
	handle(paymentHandler PaymentHandler, peerHandler PeerHandler)
}

// Payment manager manages payment requests and process actual payments over the Stellar network
type PaymentManager struct {
	paymentMessages chan paymentMessage

	ctx    			context.Context
	cancel			func()

	stellarClient	*horizonclient.Client

	network      	bsnet.BitSwapNetwork
	peerHandler  	PeerHandler
	paymentGauge 	metrics.Gauge
	keypair      	*keypair.Full

	debtRegistry	map[peer.ID]*Debt
}

func (pm *PaymentManager) getOwnStellarKey() *keypair.Full {
	return pm.keypair
}

func (pm *PaymentManager) getStellarClient() *horizonclient.Client {
	return pm.stellarClient
}

type Debt struct {
	id peer.ID

	validationQueue		*list.List

	requestedAmount		float64

	transferredBytes int
	receivedBytes int
}

const (
	megabytePrice = 0.00002 // XLM

	requestPaymentAfterBytes = 50 * 1024 * 1024 // Pey per each 50 MB including transaction fee => 50 * 0.00002 + 0.00001 = 0.00101 XLM , 1 XLM pays for 49,5GB of data
)

// New initializes a new WantManager for a given context.
func New(ctx context.Context, peerHandler PeerHandler, network bsnet.BitSwapNetwork) *PaymentManager {
	ctx, cancel := context.WithCancel(ctx)
	paymentGauge := metrics.NewCtx(ctx, "payments_total",
		"Number of items in payments queue.").Gauge()

	registry := make(map[peer.ID]*Debt)

	kp, err := keypair.ParseFull(network.GetStellarSeed())

	if err != nil {
		return nil
	}

	// Create and fund the address on TestNet, using friendbot
	client := horizonclient.DefaultTestNetClient

	return &PaymentManager{
		paymentMessages:  make(chan paymentMessage, 10),
		ctx:           	ctx,
		cancel:        	cancel,
		peerHandler:   	peerHandler,
		paymentGauge: 	paymentGauge,
		network:		network,
		keypair:		kp,
		stellarClient:	client,
		debtRegistry:	registry,
	}
}

// Startup starts processing for the PayManager.
func (pm *PaymentManager) Startup() {
	go pm.run()

	go pm.validationTimer()
}

// Shutdown ends processing for the pay manager.
func (pm *PaymentManager) Shutdown() {
	pm.cancel()
}

func (pm *PaymentManager) getPeerStellarKey(id peer.ID) (string, error) {
	key, err := pm.network.GetFromPeerStore(id, "StellarKey")

	if err != nil {
		return "", err
	}

	return key.(string), nil
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
	sourceStellarKey, err := pm.getPeerStellarKey(d.id)

	if err != nil {
		log.Error("agent version mismatch", err)
	}

	var next *list.Element
	for e := d.validationQueue.Front(); e != nil; e = next {
		hash := e.Value.(string)

		ops, err := pm.getStellarClient().Payments(horizonclient.OperationRequest{
			ForTransaction: hash,
		})

		if err != nil {
			hError := err.(*horizonclient.Error)
			log.Fatal("Error requesting transaction:", hError)
			return
		}

		removeElement := false

		for _, record := range ops.Embedded.Records {
			// check record is of type payment
			if record.GetType() == "payment" {
				payment := record.(operations.Payment)

				if payment.From != sourceStellarKey {
					// Fraud
					log.Error("Unexpected stellar source account")
					continue
				}

				if !payment.Base.TransactionSuccessful {
					log.Warning("Unsuccessful payment transaction received")
					continue
				}

				amount, err := strconv.ParseFloat(payment.Amount, 64)

				if err != nil {
					hError := err.(*horizonclient.Error)
					log.Error("Error amount parsing:", hError)
				}

				d.requestedAmount -= amount

				removeElement = true
			}
		}

		next = e.Next()

		if removeElement {
			d.validationQueue.Remove(e)
		}
	}
}

func (pm *PaymentManager) getDebt(id peer.ID) *Debt {
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

func (pm *PaymentManager) RegisterReceivedBytes(ctx context.Context, id peer.ID, msgSize int) {
	select {
	case pm.paymentMessages <- &registerReceivedBytes{target: id, msgSize: msgSize}:
	case <-pm.ctx.Done():
	case <-ctx.Done():
	}
}

func (pm *PaymentManager) RequirePayment(ctx context.Context, id peer.ID, msgSize int) {
	select {
	case pm.paymentMessages <- &requirePayment{target: id, msgSize: msgSize}:
	case <-pm.ctx.Done():
	case <-ctx.Done():
	}
}

func (pm *PaymentManager) ProcessPayment(ctx context.Context, id peer.ID, payAmount float64) {
	select {
	case pm.paymentMessages <- &processPayment{target: id, payAmount: payAmount}:
	case <-pm.ctx.Done():
	case <-ctx.Done():
	}
}

func (pm *PaymentManager) ValidatePayment(ctx context.Context, id peer.ID, paymentHash string) {
	select {
	case pm.paymentMessages <- &validatePayment{from: id, paymentHash: paymentHash}:
	case <-pm.ctx.Done():
	case <-ctx.Done():
	}
}

type registerReceivedBytes struct {
	target peer.ID
	msgSize int
}

func (r registerReceivedBytes) handle(handler PaymentHandler, peerHandler PeerHandler)  {
	debt := handler.getDebt(r.target)

	debt.receivedBytes += r.msgSize
}

type requirePayment struct {
	target peer.ID
	msgSize int
}

func (r requirePayment) handle(handler PaymentHandler, peerHandler PeerHandler) {
	debt := handler.getDebt(r.target)

	debt.transferredBytes += r.msgSize

	if debt.transferredBytes >= requestPaymentAfterBytes {
		amount := float64(debt.transferredBytes) / 1024 / 1024 * megabytePrice

		peerHandler.RequirePaymentMessage(r.target, amount)

		debt.requestedAmount += amount
		debt.transferredBytes = 0
	}
}

type validatePayment struct {
	from 		peer.ID
	paymentHash string
}

func (v validatePayment) handle(handler PaymentHandler, peerHandler PeerHandler) {
	debt := handler.getDebt(v.from)

	debt.validationQueue.PushBack(v.paymentHash)
}

type processPayment struct {
	target 		peer.ID
	payAmount	float64
}

// Processed by "client" peer
func (p processPayment) handle(pm PaymentHandler, peerHandler PeerHandler) {
	targetStellarKey, err := pm.getPeerStellarKey(p.target)

	if err != nil {
		log.Fatal("Peer stellar key not found", err)
		return
	}

	log.Debug(targetStellarKey)

	debt := pm.getDebt(p.target)

	requiredBytes := int(p.payAmount / megabytePrice * 1024 * 1024)

	if requiredBytes > debt.receivedBytes {
		log.Fatal("Peer request payment for non transferred data", err)
		return
	}

	// Account detail need to be fetch before every transaction to refresh sequence number
	ar := horizonclient.AccountRequest{AccountID: pm.getOwnStellarKey().Address()}
	sourceAccount, err := pm.getStellarClient().AccountDetail(ar)

	if err != nil {
		hError := err.(*horizonclient.Error)
		log.Fatal("Peer stellar account not found", hError)
		return
	}

	op := txnbuild.Payment{
		Destination: targetStellarKey,
		Amount:      strconv.FormatFloat(p.payAmount, 'f', -1, 64),
		Asset:       txnbuild.NativeAsset{}, // TODO: use PiedPiper asset
	}

	// Construct the transaction that will carry the operation
	tx := txnbuild.Transaction{
		SourceAccount: &sourceAccount,
		Operations:    []txnbuild.Operation{&op},
		Timebounds:    txnbuild.NewTimeout(300),
		Network:       network.TestNetworkPassphrase,
	}

	// Sign the transaction, serialise it to XDR, and base 64 encode it
	txeBase64, err := tx.BuildSignEncode(pm.getOwnStellarKey())

	// Submit the transaction
	resp, err := pm.getStellarClient().SubmitTransactionXDR(txeBase64)

	if err != nil {
		hError := err.(*horizonclient.Error)
		log.Fatal("Error submitting transaction:", hError)
		return
	}

	peerHandler.SendPaymentMessage(p.target, resp.Hash)

	debt.receivedBytes -= requiredBytes
}
