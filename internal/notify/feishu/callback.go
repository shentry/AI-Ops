package feishu

import (
	"context"
	"errors"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	larkdispatcher "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// Dispatcher is the narrow interface consumed by the HTTP adapter. The
// official EventDispatcher is the default implementation and remains
// responsible for challenge handling, signature verification and decryption.
type Dispatcher interface {
	Handle(context.Context, *larkevent.EventReq) *larkevent.EventResp
}

// CallbackHandlers contains business callbacks. They execute after the SDK
// has authenticated and decoded the event; this package does not duplicate
// signature or AES handling.
type CallbackHandlers struct {
	CardAction     func(context.Context, *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error)
	MessageReceive func(context.Context, *larkim.P2MessageReceiveV1) error
}

// NewDispatcher creates and registers the two Feishu callbacks used by the
// control room: interactive card actions and bot message reception.
func NewDispatcher(verificationToken, encryptKey string, handlers CallbackHandlers) *larkdispatcher.EventDispatcher {
	dispatcher := larkdispatcher.NewEventDispatcher(verificationToken, encryptKey)
	return RegisterHandlers(dispatcher, handlers)
}

// NewEventDispatcher is an explicit alias for assembly code that wants to
// distinguish this configured dispatcher from the SDK constructor.
func NewEventDispatcher(verificationToken, encryptKey string, handlers CallbackHandlers) *larkdispatcher.EventDispatcher {
	return NewDispatcher(verificationToken, encryptKey, handlers)
}

// NewCallbackDispatcher is the descriptive constructor used by HTTP route
// assembly code.
func NewCallbackDispatcher(verificationToken, encryptKey string, handlers CallbackHandlers) *larkdispatcher.EventDispatcher {
	return NewDispatcher(verificationToken, encryptKey, handlers)
}

// RegisterCallbacks is an alias that makes the registration step explicit.
func RegisterCallbacks(dispatcher *larkdispatcher.EventDispatcher, handlers CallbackHandlers) *larkdispatcher.EventDispatcher {
	return RegisterHandlers(dispatcher, handlers)
}

// RegisterHandlers installs callbacks on an existing official dispatcher. It
// always registers both event types so a missing business handler fails as a
// controlled callback error instead of silently returning success.
func RegisterHandlers(dispatcher *larkdispatcher.EventDispatcher, handlers CallbackHandlers) *larkdispatcher.EventDispatcher {
	if dispatcher == nil {
		return nil
	}
	cardHandler := handlers.CardAction
	if cardHandler == nil {
		cardHandler = func(context.Context, *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
			return nil, errors.New("feishu: card action handler is not configured")
		}
	}
	messageHandler := handlers.MessageReceive
	if messageHandler == nil {
		messageHandler = func(context.Context, *larkim.P2MessageReceiveV1) error {
			return errors.New("feishu: message receive handler is not configured")
		}
	}
	return dispatcher.
		OnP2CardActionTrigger(cardHandler).
		OnP2MessageReceiveV1(messageHandler)
}

// SDKDispatcher exposes the concrete type for callers that need SDK-specific
// setup while retaining Dispatcher for HTTP and test adapters.
type SDKDispatcher = larkdispatcher.EventDispatcher

type CardActionTriggerEvent = callback.CardActionTriggerEvent
type CardActionTriggerResponse = callback.CardActionTriggerResponse
type P2MessageReceiveV1 = larkim.P2MessageReceiveV1

type CallbackDispatcher = larkdispatcher.EventDispatcher
