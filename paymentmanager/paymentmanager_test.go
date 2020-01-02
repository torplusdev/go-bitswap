package paymentmanager

import (
	"container/list"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/stellar/go/clients/horizonclient"
	"github.com/stellar/go/keypair"
	"github.com/stellar/go/network"
	"github.com/stellar/go/txnbuild"
	"strconv"
	"testing"
)

type PaymentHandlerMock struct {
	keyMap map[peer.ID]*keypair.Full
	ownKey	*keypair.Full
	stellarClient	*horizonclient.Client
	debtRegistry	map[peer.ID]*Debt
}

func (p PaymentHandlerMock) getPeerStellarKey(id peer.ID) (string, error) {
	return p.keyMap[id].Address(), nil
}

func (p PaymentHandlerMock) getDebt(id peer.ID) *Debt {
	return p.debtRegistry[id]
}

func (p PaymentHandlerMock) getOwnStellarKey() *keypair.Full {
	return p.ownKey
}

func (p PaymentHandlerMock) getStellarClient() *horizonclient.Client {
	return p.stellarClient
}

type PeerHandlerMock struct {
	paymentMessages map[peer.ID]string
}

func (p PeerHandlerMock) SendPaymentMessage(target peer.ID, paymentHash string) {
	p.paymentMessages[target] = paymentHash
}

func (p PeerHandlerMock) RequirePaymentMessage(target peer.ID, amount float64) {
	panic("implement me")
}

func TestHandlePayment(t *testing.T) {
	msg := processPayment{
		target:    "targetId",
		payAmount: 10,
	}

	peerKey, _ := keypair.Random()
	ownKey, _ := keypair.Random()
	client := horizonclient.DefaultTestNetClient

	paymentMock := &PaymentHandlerMock{
		keyMap: map[peer.ID]*keypair.Full{
			"targetId": peerKey,
		},
		ownKey: ownKey,
		stellarClient: client,
		debtRegistry: map[peer.ID]*Debt{
			"targetId": &Debt{
				id:               "targetId",
				validationQueue:  nil,
				requestedAmount:  0,
				transferredBytes: 0,
				receivedBytes:    10 / megabytePrice * 1024,
			},
		},
	}

	peerMock := &PeerHandlerMock{
		paymentMessages: map[peer.ID]string{},
	}

	client.Fund(ownKey.Address())
	client.Fund(peerKey.Address())

	msg.handle(paymentMock, peerMock)

	_, ok := peerMock.paymentMessages[msg.target]

	if !ok  {
		t.Errorf("No payment hash found")
	}
}

func TestValidateTransactions(t *testing.T) {
	peerKey, _ := keypair.Random()
	ownKey, _ := keypair.Random()
	client := horizonclient.DefaultTestNetClient

	debt := Debt{
		id:               "PeerId",
		validationQueue:  list.New(),
		requestedAmount:  10,
	}

	paymentMock := &PaymentHandlerMock{
		keyMap: map[peer.ID]*keypair.Full{
			"PeerId": peerKey,
		},
		ownKey: ownKey,
		stellarClient: client,
		debtRegistry: map[peer.ID]*Debt{
			"PeerId": &debt,
		},
	}

	client.Fund(ownKey.Address())
	client.Fund(peerKey.Address())

	// Account detail need to be fetch before every transaction to refresh sequence number
	ar := horizonclient.AccountRequest{AccountID: peerKey.Address()}
	sourceAccount, err := client.AccountDetail(ar)

	if err != nil {
		hError := err.(*horizonclient.Error)
		log.Fatal("Peer stellar account not found", hError)
		return
	}

	op := txnbuild.Payment{
		Destination: ownKey.Address(),
		Amount:      strconv.FormatFloat(10, 'f', -1, 64),
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
	txeBase64, err := tx.BuildSignEncode(peerKey)

	// Submit the transaction
	resp, err := client.SubmitTransactionXDR(txeBase64)

	debt.validationQueue.PushBack(resp.Hash)

	debt.validateTransactions(paymentMock)

	if debt.requestedAmount > 0 {
		t.Errorf("Amount not validated")
	}

	if debt.validationQueue.Len() > 0 {
		t.Errorf("Validation queue not cleared")
	}
}
