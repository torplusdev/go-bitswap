package paymentmanager

import (
	"context"
)

type ClientHandler interface {
	ProcessCommand(commandId string, commandType int32, commandBody []byte, nodeId string) error
	ProcessPayment(paymentRequest string, nodeId string)
	ValidatePayment(paymentRequest string) (uint32,error)
	CreatePaymentInfo(amount int) (string, error)
	ProcessResponse(commandId string, responseBody []byte, nodeId string)
}

type CallbackHandler interface {
	Start()
	Shutdown(ctx context.Context)
}

