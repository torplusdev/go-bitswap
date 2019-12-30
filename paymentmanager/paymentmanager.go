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
	RequirePaymentMessage(target peer.ID, blocks int)
}

type paymentMessage interface {
	handle(wm *PaymentManager)
}

// Payment manager manages payment requests and process actual payments over the Stellar network
type PaymentManager struct {
	paymentMessages chan paymentMessage

	ctx    context.Context
	cancel func()

	stellarClient	*horizonclient.Client

	network      	bsnet.BitSwapNetwork
	peerHandler  	PeerHandler
	paymentGauge 	metrics.Gauge
	keypair      	*keypair.Full

	debtRegistry	map[peer.ID]*Debt
}

type Debt struct {
	validationQueue	*list.List

	requested 		float64
}

const (
	coefficient = 0.01
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
			message.handle(pm)
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

	for id, debt := range pm.debtRegistry {
		wg.Add(1)

		go func() {
			defer wg.Done()

			pm.validateTransactions(id, debt)
		}()
	}

	wg.Wait()
}

func (pm *PaymentManager) validateTransactions(id peer.ID, d *Debt) {
	sourceStellarKey, err := pm.getPeerStellarKey(id)

	if err != nil {
		log.Error("agent version mismatch", err)
	}

	var next *list.Element
	for e := d.validationQueue.Front(); e != nil; e = next {
		hash := e.Value.(string)

		ops, err := pm.stellarClient.Payments(horizonclient.OperationRequest{
			ForTransaction: hash,
		})

		if err != nil {
			hError := err.(*horizonclient.Error)
			log.Error("Error requesting transaction:", hError)
		}

		removeElement := false

		for _, record := range ops.Embedded.Records {
			// check record is of type payment
			if record.GetType() == "payment" {
				payment := record.(operations.Payment)

				if payment.From != sourceStellarKey {
					// Fraud
				}

				amount, err := strconv.ParseFloat(payment.Amount, 64)

				if err != nil {
					hError := err.(*horizonclient.Error)
					log.Error("Error amount parsing:", hError)
				}

				d.requested -= amount

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
		validationQueue: list.New(),
	}

	pm.debtRegistry[id] = debt

	return debt
}

func (pm *PaymentManager) RequirePayment(ctx context.Context, id peer.ID, blocks int) {
	select {
	case pm.paymentMessages <- &requirePayment{target: id, blocks: blocks}:
	case <-pm.ctx.Done():
	case <-ctx.Done():
	}
}

func (pm *PaymentManager) ProcessPayment(ctx context.Context, id peer.ID, payBlocks int) {
	select {
	case pm.paymentMessages <- &processPayment{target: id, payBlocks: payBlocks}:
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

type requirePayment struct {
	target peer.ID
	blocks int
}

func (r requirePayment) handle(pm *PaymentManager) {
	debt := pm.getDebt(r.target)

	debt.requested += float64(r.blocks) * coefficient

	pm.peerHandler.RequirePaymentMessage(r.target, r.blocks)
}

type validatePayment struct {
	from 		peer.ID
	paymentHash string
}

func (v validatePayment) handle(pm *PaymentManager) {
	debt := pm.getDebt(v.from)

	debt.validationQueue.PushBack(v.paymentHash)
}

type processPayment struct {
	target peer.ID
	payBlocks int
}

// Processed by "client" peer
func (p processPayment) handle(pm *PaymentManager) {
	targetStellarKey, err := pm.getPeerStellarKey(p.target)

	if err != nil {
		log.Error("agent version mismatch", err)
	}

	log.Debug(targetStellarKey)

	// Account detail need to be fetch before every transaction to refresh sequence number
	ar := horizonclient.AccountRequest{AccountID: pm.keypair.Address()}
	sourceAccount, err := pm.stellarClient.AccountDetail(ar)

	amount := float64(p.payBlocks) * coefficient // TODO: move multiplier to const

	op := txnbuild.Payment{
		Destination: targetStellarKey,
		Amount:      strconv.FormatFloat(amount, 'f', -1, 64),
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
	txeBase64, err := tx.BuildSignEncode(pm.keypair)

	// Submit the transaction
	resp, err := pm.stellarClient.SubmitTransactionXDR(txeBase64)
	if err != nil {
		hError := err.(*horizonclient.Error)
		log.Fatal("Error submitting transaction:", hError)
	}

	pm.peerHandler.SendPaymentMessage(p.target, resp.Hash)
}
