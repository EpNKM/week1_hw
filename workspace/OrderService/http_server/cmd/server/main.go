package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	orderV1 "github.com/EpNKM/week1_HW/workspace/shared/api"
)

const (
	httpPort = "8080"

	readHeaderTimeout = 5 * time.Second
	shutdownTimeout   = 10 * time.Second
)

// Order представляет заказ в памяти
type Order struct {
	OrderUUID       uuid.UUID   `json:"order_uuid"`
	UserUUID        uuid.UUID   `json:"user_uuid"`
	PartUUIDs       []uuid.UUID `json:"part_uuids"`
	TotalPrice      float64     `json:"total_price"`
	TransactionUUID *uuid.UUID  `json:"transaction_uuid,omitempty"`
	PaymentMethod   *string     `json:"payment_method,omitempty"`
	Status          orderV1.OrderResponseStatus
}

// OrderStorage — потокобезопасное хранилище заказов
type OrderStorage struct {
	mu     sync.RWMutex
	orders map[uuid.UUID]*Order
}

func NewOrderStorage() *OrderStorage {
	return &OrderStorage{
		orders: make(map[uuid.UUID]*Order),
	}
}

func (s *OrderStorage) Get(orderUUID uuid.UUID) (*Order, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	order, ok := s.orders[orderUUID]
	return order, ok
}

func (s *OrderStorage) Save(order *Order) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.orders[order.OrderUUID] = order
}

// OrderHandler реализует orderV1.Handler
type OrderHandler struct {
	storage *OrderStorage
}

func NewOrderHandler(storage *OrderStorage) *OrderHandler {
	return &OrderHandler{storage: storage}
}

// CreateOrder — обработка POST /api/v1/orders
func (h *OrderHandler) CreateOrder(_ context.Context, req *orderV1.CreateOrderRequest) (orderV1.CreateOrderRes, error) {
	if req.UserUUID == uuid.Nil {
		return &orderV1.CreateOrderBadRequest{
			Message: "user_uuid is required",
		}, nil
	}
	if len(req.PartUuids) == 0 {
		return &orderV1.CreateOrderBadRequest{
			Message: "part_uuids must not be empty",
		}, nil
	}

	totalPrice := float64(len(req.PartUuids)) * 100.0
	orderUUID := uuid.New()

	order := &Order{
		OrderUUID:  orderUUID,
		UserUUID:   req.UserUUID,
		PartUUIDs:  req.PartUuids,
		TotalPrice: totalPrice,
		Status:     orderV1.OrderResponseStatusPENDINGPAYMENT,
	}

	h.storage.Save(order)

	return &orderV1.CreateOrderResponse{
		OrderUUID:  orderUUID,
		TotalPrice: totalPrice,
	}, nil
}

// GetOrder — обработка GET /api/v1/orders/{order_uuid}
func (h *OrderHandler) GetOrder(_ context.Context, params orderV1.GetOrderParams) (orderV1.GetOrderRes, error) {
	order, ok := h.storage.Get(params.OrderUUID)
	if !ok {
		return &orderV1.GetOrderNotFound{
			Message: "Order not found",
		}, nil
	}

	resp := orderV1.OrderResponse{
		OrderUUID:  order.OrderUUID,
		UserUUID:   order.UserUUID,
		PartUuids:  order.PartUUIDs,
		TotalPrice: order.TotalPrice,
		Status:     order.Status,
	}

	if order.TransactionUUID != nil {
		resp.TransactionUUID = orderV1.NewOptNilUUID(*order.TransactionUUID)
	}
	if order.PaymentMethod != nil {
		pm := orderV1.OrderResponsePaymentMethod(*order.PaymentMethod)
		resp.PaymentMethod = orderV1.NewOptOrderResponsePaymentMethod(pm)
	}

	return &resp, nil
}

// PayOrder — обработка POST /api/v1/orders/{order_uuid}/pay
func (h *OrderHandler) PayOrder(_ context.Context, req *orderV1.PayOrderRequest, params orderV1.PayOrderParams) (orderV1.PayOrderRes, error) {
	order, ok := h.storage.Get(params.OrderUUID)
	if !ok {
		return &orderV1.PayOrderNotFound{
			Message: "Order not found",
		}, nil
	}

	if order.Status != orderV1.OrderResponseStatusPENDINGPAYMENT {
		return &orderV1.PayOrderBadRequest{
			Message: "Order is not in PENDING_PAYMENT status",
		}, nil
	}

	transactionUUID := uuid.New()
	paymentMethodStr := string(req.PaymentMethod)

	order.Status = orderV1.OrderResponseStatusPAID
	order.TransactionUUID = &transactionUUID
	order.PaymentMethod = &paymentMethodStr

	h.storage.Save(order)

	return &orderV1.PayOrderResponse{
		TransactionUUID: transactionUUID,
	}, nil
}

// CancelOrder — обработка POST /api/v1/orders/{order_uuid}/cancel
func (h *OrderHandler) CancelOrder(_ context.Context, params orderV1.CancelOrderParams) (orderV1.CancelOrderRes, error) {
	order, ok := h.storage.Get(params.OrderUUID)
	if !ok {
		return &orderV1.CancelOrderNotFound{
			Message: "Order not found",
		}, nil
	}

	if order.Status == orderV1.OrderResponseStatusPAID {
		return &orderV1.CancelOrderConflict{
			Message: "Paid order cannot be cancelled",
		}, nil
	}

	order.Status = orderV1.OrderResponseStatusCANCELLED
	h.storage.Save(order)

	return &orderV1.CancelOrderNoContent{}, nil
}

// NewError — обработка непредвиденных ошибок
func (h *OrderHandler) NewError(_ context.Context, err error) *orderV1.ApiErrorStatusCode {
	log.Printf("Unexpected error: %v", err)
	return &orderV1.ApiErrorStatusCode{
		StatusCode: 500,
		Response: orderV1.ApiError{
			Message: "Internal server error",
		},
	}
}

func main() {
	storage := NewOrderStorage()
	handler := NewOrderHandler(storage)

	server, err := orderV1.NewServer(handler)
	if err != nil {
		log.Fatalf("Failed to create OpenAPI server: %v", err)
	}

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(10 * time.Second))
	r.Mount("/api/v1", server)

	httpServer := &http.Server{
		Addr:              net.JoinHostPort("localhost", httpPort),
		Handler:           r,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	go func() {
		log.Printf("🚀 OrderService started on port %s", httpPort)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("🛑 Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("❌ Shutdown error: %v", err)
	}

	log.Println("✅ Server stopped")
}
