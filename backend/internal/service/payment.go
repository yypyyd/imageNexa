package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"backend/internal/model"
	"backend/internal/provider/epay"
	"backend/internal/repo"

	"gorm.io/gorm"
)

const orderTTL = 30 * time.Minute

var (
	ErrPayDisabled   = errors.New("recharge is currently disabled")
	ErrPayNotConfig  = errors.New("payment is not configured")
	ErrPayMethod     = errors.New("unsupported payment method")
	ErrPayAmount     = errors.New("amount below the minimum")
	ErrOrderNotFound = errors.New("order not found")
	ErrOrderPaid     = errors.New("order already paid")
)

// PaymentService drives 易支付 recharge orders: create → pay → async-notify →
// credit points. Config lives in site settings (pay.*).
type PaymentService struct {
	orders   *repo.OrderRepository
	users    *repo.UserRepository
	settings *repo.SiteSettingRepository
}

func NewPaymentService(orders *repo.OrderRepository, users *repo.UserRepository, settings *repo.SiteSettingRepository) *PaymentService {
	return &PaymentService{orders: orders, users: users, settings: settings}
}

type PaySettings struct {
	Enabled     bool     `json:"enabled"`
	PID         string   `json:"pid"`
	Key         string   `json:"key"`
	APIBase     string   `json:"api_base"` // 易支付站点根地址,代码自动拼 /api/pay/create
	Methods     []string `json:"methods"`  // wxpay, alipay
	MinAmount   float64  `json:"min_amount"`
	PointsRatio int      `json:"points_ratio"` // 积分 per 元
}

func (s *PaymentService) get(ctx context.Context, key string) string {
	v, _ := s.settings.GetValue(ctx, key)
	return v
}

func (s *PaymentService) Settings(ctx context.Context) PaySettings {
	api := strings.TrimRight(strings.TrimSpace(s.get(ctx, "pay.api_base")), "/")
	if api == "" {
		api = "https://pay.v8jisu.cn/api/pay"
	}
	ratio, _ := strconv.Atoi(s.get(ctx, "pay.points_ratio"))
	if ratio <= 0 {
		ratio = 100
	}
	minAmt, _ := strconv.ParseFloat(s.get(ctx, "pay.min_amount"), 64)
	var methods []string
	for _, m := range strings.Split(s.get(ctx, "pay.methods"), ",") {
		if m = strings.TrimSpace(m); m != "" {
			methods = append(methods, m)
		}
	}
	if len(methods) == 0 {
		methods = []string{"wxpay", "alipay"}
	}
	return PaySettings{
		Enabled:     s.get(ctx, "pay.enabled") == "true",
		PID:         s.get(ctx, "pay.pid"),
		Key:         s.get(ctx, "pay.key"),
		APIBase:     api,
		Methods:     methods,
		MinAmount:   minAmt,
		PointsRatio: ratio,
	}
}

// Public returns the recharge config for the user UI (no merchant secrets).
func (s *PaymentService) Public(ctx context.Context) map[string]any {
	c := s.Settings(ctx)
	return map[string]any{
		"enabled":      c.Enabled,
		"methods":      c.Methods,
		"min_amount":   c.MinAmount,
		"points_ratio": c.PointsRatio,
	}
}

func (s *PaymentService) SaveSettings(ctx context.Context, in PaySettings) error {
	api := strings.TrimRight(strings.TrimSpace(in.APIBase), "/")
	pid := strings.TrimSpace(in.PID)
	key := strings.TrimSpace(in.Key)
	if api == "" {
		return errors.New("支付地址不能为空")
	}
	if pid == "" {
		return errors.New("商户ID不能为空")
	}
	if key == "" {
		return errors.New("商户密钥不能为空")
	}
	if in.PointsRatio <= 0 {
		in.PointsRatio = 100
	}
	if in.MinAmount < 0 {
		in.MinAmount = 0
	}
	return s.settings.UpsertValues(ctx, map[string]string{
		"pay.enabled":      strconv.FormatBool(in.Enabled),
		"pay.pid":          pid,
		"pay.key":          key,
		"pay.api_base":     api,
		"pay.methods":      strings.Join(in.Methods, ","),
		"pay.min_amount":   strconv.FormatFloat(in.MinAmount, 'f', -1, 64),
		"pay.points_ratio": strconv.Itoa(in.PointsRatio),
	})
}

func (c PaySettings) allows(method string) bool {
	for _, m := range c.Methods {
		if m == method {
			return true
		}
	}
	return false
}

func (s *PaymentService) client(c PaySettings) *epay.Config {
	return &epay.Config{APIBase: c.APIBase, PID: c.PID, Key: c.Key}
}

// CreateOrder validates the request, persists a pending order, and places the
// 易支付 order. notifyBase is the public site origin (e.g. https://vividai.run).
func (s *PaymentService) CreateOrder(ctx context.Context, user *model.User, amount float64, method, notifyBase, clientIP string) (*model.Order, error) {
	cfg := s.Settings(ctx)
	if !cfg.Enabled {
		return nil, ErrPayDisabled
	}
	if cfg.PID == "" || cfg.Key == "" {
		return nil, ErrPayNotConfig
	}
	if !cfg.allows(method) {
		return nil, ErrPayMethod
	}
	amount = math.Round(amount*100) / 100
	if amount <= 0 || amount < cfg.MinAmount {
		return nil, ErrPayAmount
	}
	points := int(math.Round(amount * float64(cfg.PointsRatio)))
	now := time.Now()
	order := &model.Order{
		ID:        "P" + strconv.FormatInt(now.Unix(), 10) + randomUpper(6),
		UserID:    user.ID,
		Amount:    amount,
		Points:    points,
		PayType:   method,
		Status:    "pending",
		ExpiresAt: now.Add(orderTTL),
	}
	if err := s.orders.Create(ctx, order); err != nil {
		return nil, err
	}
	res, err := s.client(cfg).Create(ctx, epay.CreateRequest{
		OutTradeNo: order.ID,
		Type:       method,
		Name:       fmt.Sprintf("积分充值 %d", points),
		Money:      strconv.FormatFloat(amount, 'f', 2, 64),
		NotifyURL:  strings.TrimRight(notifyBase, "/") + "/admin/api/pay/notify",
		ClientIP:   clientIP,
	})
	if err != nil {
		_ = s.orders.Update(ctx, order.ID, map[string]any{"status": "cancelled"})
		return nil, err
	}
	order.TradeNo = res.TradeNo
	order.PayInfo = res.PayInfo
	order.PayInfoType = res.PayType
	_ = s.orders.Update(ctx, order.ID, map[string]any{
		"trade_no": res.TradeNo, "pay_info": res.PayInfo, "pay_info_type": res.PayType,
	})
	return order, nil
}

// Continue re-creates payment for an unpaid order (clones its amount/method into
// a fresh order — keeps the old one as history). Used by 订单管理 "继续支付".
func (s *PaymentService) Continue(ctx context.Context, user *model.User, orderID, notifyBase, clientIP string) (*model.Order, error) {
	old, err := s.orders.Get(ctx, orderID)
	if err != nil || old == nil || old.UserID != user.ID {
		return nil, ErrOrderNotFound
	}
	if old.Status == "paid" {
		return nil, ErrOrderPaid
	}
	// Reuse a still-valid pending order — return its saved qrcode/payurl directly,
	// no new upstream order (and the countdown keeps its original expiry).
	if old.Status == "pending" && old.PayInfo != "" && time.Now().Before(old.ExpiresAt) {
		return old, nil
	}
	// Expired / cancelled (or never got a pay_info) → place a fresh order.
	return s.CreateOrder(ctx, user, old.Amount, old.PayType, notifyBase, clientIP)
}

// HandleNotify processes an 易支付 async callback. Returns true when this call is
// the one that credited the user (first successful notify for the order).
func (s *PaymentService) HandleNotify(ctx context.Context, params map[string]string) (bool, error) {
	cfg := s.Settings(ctx)
	if cfg.Key == "" {
		return false, ErrPayNotConfig
	}
	if !s.client(cfg).VerifyNotify(params) {
		return false, errors.New("bad sign")
	}
	if params["trade_status"] != "TRADE_SUCCESS" {
		return false, nil
	}
	orderID := params["out_trade_no"]
	order, err := s.orders.Get(ctx, orderID)
	if err != nil || order == nil {
		return false, ErrOrderNotFound
	}
	credited, err := s.orders.MarkPaid(ctx, order.ID, params["trade_no"], time.Now())
	if err != nil {
		return false, err
	}
	if !credited {
		return false, nil // duplicate / already handled
	}
	// Credit points + bump the user's cumulative recharge total, atomically.
	_, err = s.users.Update(ctx, order.UserID, map[string]any{
		"credits":        gorm.Expr("credits + ?", order.Points),
		"recharge_total": gorm.Expr("recharge_total + ?", order.Amount),
	})
	return err == nil, err
}

func (s *PaymentService) GetForUser(ctx context.Context, userID, orderID string) (*model.Order, error) {
	o, err := s.orders.Get(ctx, orderID)
	if err != nil || o == nil || o.UserID != userID {
		return nil, ErrOrderNotFound
	}
	return o, nil
}

func (s *PaymentService) ListByUser(ctx context.Context, userID, status, query string, limit, offset int) ([]model.Order, int64, error) {
	return s.orders.ListByUser(ctx, userID, status, query, limit, offset)
}

// ListAll — admin order list. A search query also matches 用户名/邮箱: resolve
// the term to user ids first so "张三" finds that user's orders.
func (s *PaymentService) ListAll(ctx context.Context, status, source, query string, limit, offset int) ([]model.Order, int64, error) {
	var userIDs []string
	if term := strings.ToLower(strings.TrimSpace(query)); term != "" {
		if users, err := s.users.List(ctx); err == nil {
			for _, u := range users {
				if strings.Contains(strings.ToLower(u.Name), term) ||
					strings.Contains(strings.ToLower(u.Email), term) ||
					strings.Contains(strings.ToLower(u.ID), term) {
					userIDs = append(userIDs, u.ID)
				}
			}
		}
	}
	return s.orders.List(ctx, status, source, query, userIDs, limit, offset)
}

// RecordCreditOrder persists a synthetic already-paid order for a credit grant
// that didn't go through epay — admin manual adjustments (source="admin") and
// CDK redemptions (source="cdk") — so 订单管理 lists them alongside recharges.
// Best-effort: a failed insert must never fail the credit operation itself.
func RecordCreditOrder(ctx context.Context, orders *repo.OrderRepository, userID string, points float64, source, remark string) {
	if orders == nil {
		return
	}
	prefix := "X"
	switch source {
	case "admin":
		prefix = "M"
	case "cdk":
		prefix = "C"
	}
	now := time.Now()
	_ = orders.Create(ctx, &model.Order{
		ID:        prefix + strconv.FormatInt(now.Unix(), 10) + randomUpper(6),
		UserID:    userID,
		Points:    int(math.Round(points)),
		PayType:   source,
		Status:    "paid",
		Source:    source,
		Remark:    remark,
		ExpiresAt: now,
		PaidAt:    &now,
	})
}

// UserNames maps user id → display name (name, else email, else id) so the admin
// 订单管理 list can show 用户名.
func (s *PaymentService) UserNames(ctx context.Context) map[string]string {
	out := map[string]string{}
	users, err := s.users.List(ctx)
	if err != nil {
		return out
	}
	for _, u := range users {
		n := u.Name
		if n == "" {
			n = u.Email
		}
		if n == "" {
			n = u.ID
		}
		out[u.ID] = n
	}
	return out
}
