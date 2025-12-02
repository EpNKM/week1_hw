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

	orderV1 "github.com/EpNKM/shared/api"
)

const (
	httpPort = "8080"

	readHeaderTimeout = 5 * time.Second
	shutdownTimeout   = 10 * time.Second
)

// OrderStatus константы
const (
	StatusPendingPayment = "PENDING_PAYMENT"
	StatusPaid           = "PAID"
	StatusCancelled      = "CANCELLED"
)

// PaymentMethod константы (соответствуют OpenAPI enum)
const (
	PaymentMethodUnknown       = "UNKNOWN"
	PaymentMethodCard          = "CARD"
	PaymentMethodSBP           = "SBP"
	PaymentMethodCreditCard    = "CREDIT_CARD"
	PaymentMethodInvestorMoney = "INVESTOR_MONEY"
)

// Order представляет заказ в памяти
type Order struct {
	OrderUUID       string   `json:"order_uuid"`
	UserUUID        string   `json:"user_uuid"`
	PartUUIDs       []string `json:"part_uuids"`
	TotalPrice      float64  `json:"total_price"`
	TransactionUUID *string  `json:"transaction_uuid,omitempty"`
	PaymentMethod   *string  `json:"payment_method,omitempty"`
	Status          string   `json:"status"`
}

// OrderStorage — потокобезопасное хранилище заказов
type OrderStorage struct {
	mu     sync.RWMutex
	orders map[string]*Order
}

func NewOrderStorage() *OrderStorage {
	return &OrderStorage{
		orders: make(map[string]*Order),
	}
}

func (s *OrderStorage) Get(orderUUID string) (*Order, bool) {
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
func (h *OrderHandler) CreateOrder(_ context.Context, req orderV1.CreateOrderJSONRequestBody) (orderV1.CreateOrderRes, error) {
	// Валидация входных данных
	if req.UserUuid == nil || *req.UserUuid == "" {
		return &orderV1.BadRequestError{
			Code:    400,
			Message: "user_uuid is required",
		}, nil
	}
	if len(req.PartUuids) == 0 {
		return &orderV1.BadRequestError{
			Code:    400,
			Message: "part_uuids must not be empty",
		}, nil
	}

	// TODO: Здесь должна быть интеграция с InventoryService
	// Для упрощения домашнего задания считаем, что все детали существуют
	// и каждая стоит 100.0 единиц
	totalPrice := float64(len(req.PartUuids)) * 100.0

	orderUUID := uuid.New().String()
	order := &Order{
		OrderUUID:  orderUUID,
		UserUUID:   *req.UserUuid,
		PartUUIDs:  req.PartUuids,
		TotalPrice: totalPrice,
		Status:     StatusPendingPayment,
	}

	h.storage.Save(order)

	return &orderV1.CreateOrderResponse{
		OrderUuid:  orderUUID,
		TotalPrice: totalPrice,
	}, nil
}

// GetOrder — обработка GET /api/v1/orders/{order_uuid}
func (h *OrderHandler) GetOrder(_ context.Context, request orderV1.GetOrderRequestObject) (orderV1.GetOrderRes, error) {
	order, ok := h.storage.Get(request.OrderUuid)
	if !ok {
		return &orderV1.NotFoundError{
			Code:    404,
			Message: "Order not found",
		}, nil
	}

	resp := &orderV1.OrderResponse{
		OrderUuid:  order.OrderUUID,
		UserUuid:   order.UserUUID,
		PartUuids:  order.PartUUIDs,
		TotalPrice: order.TotalPrice,
		Status:     order.Status,
	}

	if order.TransactionUUID != nil {
		resp.TransactionUuid = order.TransactionUUID
	}
	if order.PaymentMethod != nil {
		resp.PaymentMethod = order.PaymentMethod
	}

	return resp, nil
}

// PayOrder — обработка POST /api/v1/orders/{order_uuid}/pay
func (h *OrderHandler) PayOrder(_ context.Context, request orderV1.PayOrderRequestObject) (orderV1.PayOrderRes, error) {
	order, ok := h.storage.Get(request.OrderUuid)
	if !ok {
		return &orderV1.NotFoundError{
			Code:    404,
			Message: "Order not found",
		}, nil
	}

	if order.Status != StatusPendingPayment {
		return &orderV1.BadRequestError{
			Code:    400,
			Message: "Order is not in PENDING_PAYMENT status",
		}, nil
	}

	// TODO: Вызов PaymentService.PayOrder
	// Для демонстрации генерируем transaction_uuid
	transactionUUID := uuid.New().String()

	// Обновляем заказ
	order.Status = StatusPaid
	order.TransactionUUID = &transactionUUID
	order.PaymentMethod = &request.Body.PaymentMethod

	h.storage.Save(order)

	return &orderV1.PayOrderResponse{
		TransactionUuid: transactionUUID,
	}, nil
}

// CancelOrder — обработка POST /api/v1/orders/{order_uuid}/cancel
func (h *OrderHandler) CancelOrder(_ context.Context, request orderV1.CancelOrderRequestObject) (orderV1.CancelOrderRes, error) {
	order, ok := h.storage.Get(request.OrderUuid)
	if !ok {
		return &orderV1.NotFoundError{
			Code:    404,
			Message: "Order not found",
		}, nil
	}

	if order.Status == StatusPaid {
		return &orderV1.ConflictError{
			Code:    409,
			Message: "Paid order cannot be cancelled",
		}, nil
	}

	order.Status = StatusCancelled
	h.storage.Save(order)

	return orderV1.CancelOrder204Response{}, nil
}

// NewError — обработка непредвиденных ошибок
func (h *OrderHandler) NewError(_ context.Context, err error) *orderV1.GenericErrorStatusCode {
	log.Printf("Unexpected error: %v", err)
	return &orderV1.GenericErrorStatusCode{
		StatusCode: http.StatusInternalServerError,
		Response: orderV1.GenericError{
			Code:    orderV1.NewOptInt(http.StatusInternalServerError),
			Message: orderV1.NewOptString("Internal server error"),
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
	r.Mount("/api/v1", server) // Все пути уже начинаются с /api/v1 в OpenAPI

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
