package controller

import (
	"fmt"
	"log"
	"net/url"
	"one-api/common"
	"one-api/model"
	"one-api/service"
	"one-api/setting"
	"strconv"
	"sync"
	"time"

	"github.com/Calcium-Ion/go-epay/epay"
	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
	"github.com/shopspring/decimal"
)

type EpayRequest struct {
	Amount        int64  `json:"amount"`
	PaymentMethod string `json:"payment_method"`
	TopUpCode     string `json:"top_up_code"`
}

type AmountRequest struct {
	Amount    int64  `json:"amount"`
	TopUpCode string `json:"top_up_code"`
}

func GetEpayClient() *epay.Client {
	if setting.PayAddress == "" || setting.EpayId == "" || setting.EpayKey == "" {
		return nil
	}
	withUrl, err := epay.NewClient(&epay.Config{
		PartnerID: setting.EpayId,
		Key:       setting.EpayKey,
	}, setting.PayAddress)
	if err != nil {
		return nil
	}
	return withUrl
}

func getPayMoney(amount int64, group string) float64 {
	dAmount := decimal.NewFromInt(amount)

	if !common.DisplayInCurrencyEnabled {
		dQuotaPerUnit := decimal.NewFromFloat(common.QuotaPerUnit)
		dAmount = dAmount.Div(dQuotaPerUnit)
	}

	topupGroupRatio := common.GetTopupGroupRatio(group)
	if topupGroupRatio == 0 {
		topupGroupRatio = 1
	}

	dTopupGroupRatio := decimal.NewFromFloat(topupGroupRatio)
	dPrice := decimal.NewFromFloat(setting.Price)

	payMoney := dAmount.Mul(dPrice).Mul(dTopupGroupRatio)

	return payMoney.InexactFloat64()
}

func getMinTopup() int64 {
	minTopup := setting.MinTopUp
	if !common.DisplayInCurrencyEnabled {
		dMinTopup := decimal.NewFromInt(int64(minTopup))
		dQuotaPerUnit := decimal.NewFromFloat(common.QuotaPerUnit)
		minTopup = int(dMinTopup.Mul(dQuotaPerUnit).IntPart())
	}
	return int64(minTopup)
}

func RequestEpay(c *gin.Context) {
	var req EpayRequest
	err := c.ShouldBindJSON(&req)
	if err != nil {
		c.JSON(200, gin.H{"message": "error", "data": "参数错误"})
		return
	}
	if req.Amount < getMinTopup() {
		c.JSON(200, gin.H{"message": "error", "data": fmt.Sprintf("充值数量不能小于 %d", getMinTopup())})
		return
	}

	id := c.GetInt("id")
	group, err := model.GetUserGroup(id, true)
	if err != nil {
		c.JSON(200, gin.H{"message": "error", "data": "获取用户分组失败"})
		return
	}
	payMoney := getPayMoney(req.Amount, group)
	if payMoney < 0.01 {
		c.JSON(200, gin.H{"message": "error", "data": "充值金额过低"})
		return
	}
	payType := "wxpay"
	if req.PaymentMethod == "zfb" {
		payType = "alipay"
	}
	if req.PaymentMethod == "wx" {
		req.PaymentMethod = "wxpay"
		payType = "wxpay"
	}
	callBackAddress := service.GetCallbackAddress()
	returnUrl, _ := url.Parse(setting.ServerAddress + "/log")
	notifyUrl, _ := url.Parse(callBackAddress + "/api/user/epay/notify")
	tradeNo := fmt.Sprintf("%s%d", common.GetRandomString(6), time.Now().Unix())
	tradeNo = fmt.Sprintf("USR%dNO%s", id, tradeNo)
	client := GetEpayClient()
	if client == nil {
		c.JSON(200, gin.H{"message": "error", "data": "当前管理员未配置支付信息"})
		return
	}
	uri, params, err := client.Purchase(&epay.PurchaseArgs{
		Type:           payType,
		ServiceTradeNo: tradeNo,
		Name:           fmt.Sprintf("TUC%d", req.Amount),
		Money:          strconv.FormatFloat(payMoney, 'f', 2, 64),
		Device:         epay.PC,
		NotifyUrl:      notifyUrl,
		ReturnUrl:      returnUrl,
	})
	if err != nil {
		c.JSON(200, gin.H{"message": "error", "data": "拉起支付失败"})
		return
	}
	amount := req.Amount
	if !common.DisplayInCurrencyEnabled {
		dAmount := decimal.NewFromInt(int64(amount))
		dQuotaPerUnit := decimal.NewFromFloat(common.QuotaPerUnit)
		amount = dAmount.Div(dQuotaPerUnit).IntPart()
	}
	topUp := &model.TopUp{
		UserId:     id,
		Amount:     amount,
		Money:      payMoney,
		TradeNo:    tradeNo,
		CreateTime: time.Now().Unix(),
		Status:     "pending",
	}
	err = topUp.Insert()
	if err != nil {
		c.JSON(200, gin.H{"message": "error", "data": "创建订单失败"})
		return
	}
	c.JSON(200, gin.H{"message": "success", "data": params, "url": uri})
}

// tradeNo lock
var orderLocks sync.Map
var createLock sync.Mutex

// LockOrder 尝试对给定订单号加锁
func LockOrder(tradeNo string) {
	lock, ok := orderLocks.Load(tradeNo)
	if !ok {
		createLock.Lock()
		defer createLock.Unlock()
		lock, ok = orderLocks.Load(tradeNo)
		if !ok {
			lock = new(sync.Mutex)
			orderLocks.Store(tradeNo, lock)
		}
	}
	lock.(*sync.Mutex).Lock()
}

// UnlockOrder 释放给定订单号的锁
func UnlockOrder(tradeNo string) {
	lock, ok := orderLocks.Load(tradeNo)
	if ok {
		lock.(*sync.Mutex).Unlock()
	}
}

func EpayNotify(c *gin.Context) {
	params := lo.Reduce(lo.Keys(c.Request.URL.Query()), func(r map[string]string, t string, i int) map[string]string {
		r[t] = c.Request.URL.Query().Get(t)
		return r
	}, map[string]string{})
	client := GetEpayClient()
	if client == nil {
		log.Println("易支付回调失败 未找到配置信息")
		_, err := c.Writer.Write([]byte("fail"))
		if err != nil {
			log.Println("易支付回调写入失败")
			return
		}
	}
	verifyInfo, err := client.Verify(params)
	if err == nil && verifyInfo.VerifyStatus {
		_, err := c.Writer.Write([]byte("success"))
		if err != nil {
			log.Println("易支付回调写入失败")
		}
	} else {
		_, err := c.Writer.Write([]byte("fail"))
		if err != nil {
			log.Println("易支付回调写入失败")
		}
		log.Println("易支付回调签名验证失败")
		return
	}

	if verifyInfo.TradeStatus == epay.StatusTradeSuccess {
		log.Println(verifyInfo)
		LockOrder(verifyInfo.ServiceTradeNo)
		defer UnlockOrder(verifyInfo.ServiceTradeNo)
		topUp := model.GetTopUpByTradeNo(verifyInfo.ServiceTradeNo)
		if topUp == nil {
			log.Printf("易支付回调未找到订单: %v", verifyInfo)
			return
		}
		if topUp.Status == "pending" {
			topUp.Status = "success"
			err := topUp.Update()
			if err != nil {
				log.Printf("易支付回调更新订单失败: %v", topUp)
				return
			}
			//user, _ := model.GetUserById(topUp.UserId, false)
			//user.Quota += topUp.Amount * 500000

			// 获取实际应该增加的额度
			actualAmount := getBonusQuota(topUp.Amount)
			dAmount := decimal.NewFromInt(int64(actualAmount))

			// 检查是否有奖励
			hasBonus := actualAmount > topUp.Amount

			dQuotaPerUnit := decimal.NewFromFloat(common.QuotaPerUnit)
			quotaToAdd := int(dAmount.Mul(dQuotaPerUnit).IntPart())
			err = model.IncreaseUserQuota(topUp.UserId, quotaToAdd, true)
			if err != nil {
				log.Printf("易支付回调更新用户失败: %v", topUp)
				return
			}
			log.Printf("易支付回调更新用户成功 %v", topUp)

			// 根据是否有奖励来显示不同的充值记录
			if hasBonus {
				model.RecordLog(topUp.UserId, model.LogTypeTopup, fmt.Sprintf("使用在线充值成功，充值金额: %v，支付金额：%f，活动奖励：%v", common.LogQuota(quotaToAdd), topUp.Money, common.LogQuota(int(actualAmount-topUp.Amount)*int(common.QuotaPerUnit))))
			} else {
				model.RecordLog(topUp.UserId, model.LogTypeTopup, fmt.Sprintf("使用在线充值成功，充值金额: %v，支付金额：%f", common.LogQuota(quotaToAdd), topUp.Money))
			}
		}
	} else {
		log.Printf("易支付异常回调: %v", verifyInfo)
	}
}

// getBonusRate 根据充值金额返回奖励倍率
func getBonusRate(amount int64) float64 {
	// 充值活动规则列表
	// 可以根据需要修改这些规则
	type TopupRule struct {
		MinAmount int64   // 最小充值金额
		BonusRate float64 // 奖励倍率，例如1.2表示赠送20%
	}

	rules := []TopupRule{
		{MinAmount: 1000, BonusRate: 1.2},  // 充值1000，赠送20%
		{MinAmount: 5000, BonusRate: 1.5},  // 充值5000，赠送50%
		{MinAmount: 10000, BonusRate: 2.0}, // 充值10000，赠送100%
	}

	// 从大到小检查规则，匹配第一个符合条件的规则
	for i := len(rules) - 1; i >= 0; i-- {
		if amount >= rules[i].MinAmount {
			return rules[i].BonusRate
		}
	}

	// 没有匹配的规则，返回1.0表示没有奖励
	return 1.0
}

// getBonusQuota 根据充值金额返回实际应该获得的额度
func getBonusQuota(amount int64) int64 {
	// 充值活动规则列表 - 指定金额兑换指定额度
	type TopupRule struct {
		Amount    int64 // 充值金额
		GetAmount int64 // 实际获得的额度
	}

	rules := []TopupRule{
		{Amount: 1, GetAmount: 2},     // 充值1，实际获得2
		{Amount: 50, GetAmount: 60},   // 充值50，实际获得60
		{Amount: 130, GetAmount: 180}, // 充值130，实际获得180
		{Amount: 230, GetAmount: 360}, // 充值230，实际获得360
		{Amount: 450, GetAmount: 720}, // 充值450，实际获得720
	}

	// 精确匹配
	for _, rule := range rules {
		if amount == rule.Amount {
			return rule.GetAmount
		}
	}

	// 没有匹配的规则，返回原始金额
	return amount
}

func RequestAmount(c *gin.Context) {
	var req AmountRequest
	err := c.ShouldBindJSON(&req)
	if err != nil {
		c.JSON(200, gin.H{"message": "error", "data": "参数错误"})
		return
	}

	if req.Amount < getMinTopup() {
		c.JSON(200, gin.H{"message": "error", "data": fmt.Sprintf("充值数量不能小于 %d", getMinTopup())})
		return
	}
	id := c.GetInt("id")
	group, err := model.GetUserGroup(id, true)
	if err != nil {
		c.JSON(200, gin.H{"message": "error", "data": "获取用户分组失败"})
		return
	}
	payMoney := getPayMoney(req.Amount, group)
	if payMoney <= 0.01 {
		c.JSON(200, gin.H{"message": "error", "data": "充值金额过低"})
		return
	}
	c.JSON(200, gin.H{"message": "success", "data": strconv.FormatFloat(payMoney, 'f', 2, 64)})
}
