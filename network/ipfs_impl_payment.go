package network

import (
	"context"
	"fmt"

	bsmsg "github.com/ipfs/go-bitswap/message"
	"github.com/ipfs/go-bitswap/pptools/speedcontrol"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/routing"
)

type implWithPay struct {
	impl
	speedController *speedcontrol.MultiSpeedDetector
	payOption       PPOption
}
type PPOption struct {
	CommandListenPort int
	ChannelUrl        string
}

func NewPaymentFromIpfsHost(host host.Host, r routing.ContentRouting, payOption PPOption, opts ...NetOpt) BitSwapNetwork {
	bitswapNetwork := NewFromIpfsHost(host, r, opts...)
	implNetwork, ok := bitswapNetwork.(*impl)
	if ok {
		return &implWithPay{
			impl:            *implNetwork,
			speedController: speedcontrol.NewSpeedDetector(),
			payOption:       payOption,
		}
	}
	panic("not valid implementation")
}

func (bsnet *implWithPay) newPaymentStreamToPeer(ctx context.Context, id peer.ID) (network.Stream, error) {
	stream, err := bsnet.newStreamToPeer(ctx, id)
	if err != nil {
		return nil, err
	}
	return bsnet.speedController.WrapStream(id, stream), nil
}

func (bsnet *implWithPay) SendMessage(
	ctx context.Context,
	p peer.ID,
	outgoing bsmsg.BitSwapMessage) error {
	if s, ok := outgoing.(bsmsg.PaymentBitSwapMessage); ok {
		fmt.Println("Send ", s.String())
	}
	s, err := bsnet.newPaymentStreamToPeer(ctx, p)
	if err != nil {
		fmt.Print("Stream not found")
		return err
	}

	if err = bsnet.msgToStream(ctx, s, outgoing, sendMessageTimeout); err != nil {
		_ = s.Reset()
		return err
	}

	return s.Close()
}
