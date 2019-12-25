package paymentmanager

import (
	"context"

	logging "github.com/ipfs/go-log"
	"github.com/ipfs/go-metrics-interface"

	bsnet "github.com/ipfs/go-bitswap/network"
	"github.com/libp2p/go-libp2p-core/peer"

	"github.com/stellar/go/clients/horizonclient"
	"github.com/stellar/go/keypair"
	"github.com/stellar/go/network"
	"github.com/stellar/go/txnbuild"
)

var log = logging.Logger("bitswap")

// PeerHandler sends changes out to the network as they get added to the payment list
type PeerHandler interface {
	SendPaymentMessage(target peer.ID, paymentHash string)
}

type paymentMessage interface {
	handle(wm *PaymentManager)
}

// Payment manager manages payment requests and process actual payments over the Stellar network
type PaymentManager struct {
	paymentMessages chan paymentMessage

	ctx    context.Context
	cancel func()

	network      bsnet.BitSwapNetwork
	peerHandler  PeerHandler
	paymentGauge metrics.Gauge
	keypair      *keypair.Full
}

// New initializes a new WantManager for a given context.
func New(ctx context.Context, peerHandler PeerHandler, network bsnet.BitSwapNetwork) *PaymentManager {
	ctx, cancel := context.WithCancel(ctx)
	paymentGauge := metrics.NewCtx(ctx, "payments_total",
		"Number of items in payments queue.").Gauge()

	kp, err := keypair.ParseFull(network.GetStellarSeed())

	if err != nil {
		return nil;
	}

	return &PaymentManager{
		paymentMessages:  make(chan paymentMessage, 10),
		ctx:           	ctx,
		cancel:        	cancel,
		peerHandler:   	peerHandler,
		paymentGauge: 	paymentGauge,
		network:		network,
		keypair:		kp,
	}
}


// Startup starts processing for the PayManager.
func (pm *PaymentManager) Startup() {
	go pm.run()
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

func (pm *PaymentManager) RequirePayment(ctx context.Context, id peer.ID, msgSize int) {
	select {
	case pm.paymentMessages <- &requirePayment{target: id, msgSize: msgSize}:
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
	case pm.paymentMessages <- &validatePayment{target: id, paymentHash: paymentHash}:
	case <-pm.ctx.Done():
	case <-ctx.Done():
	}
}

type requirePayment struct {
	target peer.ID
	msgSize int
}

func (r requirePayment) handle(wm *PaymentManager) {

}

type validatePayment struct {
	target peer.ID
	paymentHash string
}

func (v validatePayment) handle(wm *PaymentManager) {
	// TODO: call API

	// If payed clear wait list
}

type processPayment struct {
	target peer.ID
	payBlocks int
}

func (p processPayment) handle(wm *PaymentManager) {
	targetStellarKey, err := wm.getPeerStellarKey(p.target)
	if err != nil {
		log.Error("agent version mismatch", err)
	}

	log.Debug(targetStellarKey)

	// Create and fund the address on TestNet, using friendbot
	client := horizonclient.DefaultTestNetClient

	ar := horizonclient.AccountRequest{AccountID: wm.keypair.Address()}
	sourceAccount, err := client.AccountDetail(ar)

	op := txnbuild.Payment{
		Destination: targetStellarKey,
		Amount:      "10",
		Asset:       txnbuild.NativeAsset{},
	}

	// Construct the transaction that will carry the operation
	tx := txnbuild.Transaction{
		SourceAccount: &sourceAccount,
		Operations:    []txnbuild.Operation{&op},
		Timebounds:    txnbuild.NewTimeout(300),
		Network:       network.TestNetworkPassphrase,
	}

	// Sign the transaction, serialise it to XDR, and base 64 encode it
	txeBase64, err := tx.BuildSignEncode(wm.keypair)

	// Submit the transaction
	resp, err := client.SubmitTransactionXDR(txeBase64)
	if err != nil {
		hError := err.(*horizonclient.Error)
		log.Fatal("Error submitting transaction:", hError)
	}

	wm.peerHandler.SendPaymentMessage(p.target, resp.Hash)
}
