package wecom

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"weknora-tokendance/config"
)

type Client struct {
	cfg        config.WeComConfig
	httpClient *http.Client

	// Token 缓存
	appTokenLock sync.RWMutex
	appToken     string
	appTokenExp  time.Time

	contactTokenLock sync.RWMutex
	contactToken     string
	contactTokenExp  time.Time
}

func NewClient(cfg config.WeComConfig) *Client {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	return &Client{
		cfg: cfg,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   10 * time.Second,
		},
	}
}

// 通用 WeCom 返回基础结构
type BaseResponse struct {
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
}

// 部门定义
type Department struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	ParentID int64  `json:"parentid"`
	Order    int    `json:"order"`
}

type DepartmentListResponse struct {
	BaseResponse
	Department []Department `json:"department"`
}

// 部门成员详细信息
type UserDetail struct {
	UserID     string  `json:"userid"`
	Name       string  `json:"name"`
	Department []int64 `json:"department"`
	Position   string  `json:"position"`
	Mobile     string  `json:"mobile"`
	Telephone  string  `json:"telephone"`
	Email      string  `json:"email"`
	BizMail    string  `json:"biz_mail"`
	Avatar     string  `json:"avatar"`
	Status     int     `json:"status"` // 1=已激活，2=已禁用，4=未激活，5=退出企业
	IsLeader   []int   `json:"is_leader_in_dept"`
}

type UserListResponse struct {
	BaseResponse
	UserList []UserDetail `json:"userlist"`
}

// OAuth2 根据 code 获取用户信息
type OAuthUserResponse struct {
	BaseResponse
	UserID   string `json:"UserId"`
	OpenID   string `json:"OpenId"`
	UserTicket string `json:"user_ticket"`
}

// 获取通讯录 AccessToken
func (c *Client) GetContactAccessToken() (string, error) {
	c.contactTokenLock.RLock()
	if c.contactToken != "" && time.Now().Before(c.contactTokenExp) {
		token := c.contactToken
		c.contactTokenLock.RUnlock()
		return token, nil
	}
	c.contactTokenLock.RUnlock()

	c.contactTokenLock.Lock()
	defer c.contactTokenLock.Unlock()

	// Double check
	if c.contactToken != "" && time.Now().Before(c.contactTokenExp) {
		return c.contactToken, nil
	}

	secret := c.cfg.ContactSecret
	if secret == "" {
		secret = c.cfg.Secret
	}

	token, exp, err := c.fetchAccessToken(c.cfg.CorpID, secret)
	if err != nil {
		return "", err
	}
	c.contactToken = token
	c.contactTokenExp = exp
	return token, nil
}

// 获取自建应用 AccessToken (用于 OAuth)
func (c *Client) GetAppAccessToken() (string, error) {
	c.appTokenLock.RLock()
	if c.appToken != "" && time.Now().Before(c.appTokenExp) {
		token := c.appToken
		c.appTokenLock.RUnlock()
		return token, nil
	}
	c.appTokenLock.RUnlock()

	c.appTokenLock.Lock()
	defer c.appTokenLock.Unlock()

	if c.appToken != "" && time.Now().Before(c.appTokenExp) {
		return c.appToken, nil
	}

	token, exp, err := c.fetchAccessToken(c.cfg.CorpID, c.cfg.Secret)
	if err != nil {
		return "", err
	}
	c.appToken = token
	c.appTokenExp = exp
	return token, nil
}

func (c *Client) fetchAccessToken(corpID, secret string) (string, time.Time, error) {
	reqURL := fmt.Sprintf("https://qyapi.weixin.qq.com/cgi-bin/gettoken?corpid=%s&corpsecret=%s",
		url.QueryEscape(corpID), url.QueryEscape(secret))

	resp, err := c.httpClient.Get(reqURL)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("http request gettoken: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		BaseResponse
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", time.Time{}, fmt.Errorf("decode gettoken response: %w", err)
	}

	if result.ErrCode != 0 {
		return "", time.Time{}, fmt.Errorf("wecom gettoken failed [%d]: %s", result.ErrCode, result.ErrMsg)
	}

	// 提前 300 秒过期以确保安全
	exp := time.Now().Add(time.Duration(result.ExpiresIn-300) * time.Second)
	return result.AccessToken, exp, nil
}

// ListDepartments 拉取部门列表
func (c *Client) ListDepartments(parentDeptID int64) ([]Department, error) {
	token, err := c.GetContactAccessToken()
	if err != nil {
		return nil, err
	}

	reqURL := fmt.Sprintf("https://qyapi.weixin.qq.com/cgi-bin/department/list?access_token=%s", url.QueryEscape(token))
	if parentDeptID > 0 {
		reqURL += fmt.Sprintf("&id=%d", parentDeptID)
	}

	resp, err := c.httpClient.Get(reqURL)
	if err != nil {
		return nil, fmt.Errorf("request department/list: %w", err)
	}
	defer resp.Body.Close()

	var result DepartmentListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode department/list response: %w", err)
	}

	if result.ErrCode != 0 {
		return nil, fmt.Errorf("wecom department/list failed [%d]: %s", result.ErrCode, result.ErrMsg)
	}

	return result.Department, nil
}

// ListDepartmentUsers 拉取指定部门下的成员详情 (fetch_child=1 递归获取)
func (c *Client) ListDepartmentUsers(deptID int64, fetchChild bool) ([]UserDetail, error) {
	token, err := c.GetContactAccessToken()
	if err != nil {
		return nil, err
	}

	childFlag := "0"
	if fetchChild {
		childFlag = "1"
	}

	reqURL := fmt.Sprintf("https://qyapi.weixin.qq.com/cgi-bin/user/list?access_token=%s&department_id=%d&fetch_child=%s",
		url.QueryEscape(token), deptID, childFlag)

	resp, err := c.httpClient.Get(reqURL)
	if err != nil {
		return nil, fmt.Errorf("request user/list: %w", err)
	}
	defer resp.Body.Close()

	var result UserListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode user/list response: %w", err)
	}

	if result.ErrCode != 0 {
		return nil, fmt.Errorf("wecom user/list failed [%d]: %s", result.ErrCode, result.ErrMsg)
	}

	return result.UserList, nil
}

// GetUserInfoByCode 企微 OAuth2 回调通过 code 换取身份
func (c *Client) GetUserInfoByCode(code string) (*OAuthUserResponse, error) {
	token, err := c.GetAppAccessToken()
	if err != nil {
		return nil, err
	}

	reqURL := fmt.Sprintf("https://qyapi.weixin.qq.com/cgi-bin/auth/getuserinfo?access_token=%s&code=%s",
		url.QueryEscape(token), url.QueryEscape(code))

	resp, err := c.httpClient.Get(reqURL)
	if err != nil {
		return nil, fmt.Errorf("request auth/getuserinfo: %w", err)
	}
	defer resp.Body.Close()

	var result OAuthUserResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode getuserinfo response: %w", err)
	}

	if result.ErrCode != 0 {
		return nil, fmt.Errorf("wecom auth/getuserinfo failed [%d]: %s", result.ErrCode, result.ErrMsg)
	}

	return &result, nil
}

// GetUserDetailByID 根据 UserID 查询企微用户详情
func (c *Client) GetUserDetailByID(userID string) (*UserDetail, error) {
	token, err := c.GetContactAccessToken()
	if err != nil {
		return nil, err
	}

	reqURL := fmt.Sprintf("https://qyapi.weixin.qq.com/cgi-bin/user/get?access_token=%s&userid=%s",
		url.QueryEscape(token), url.QueryEscape(userID))

	resp, err := c.httpClient.Get(reqURL)
	if err != nil {
		return nil, fmt.Errorf("request user/get: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		BaseResponse
		UserDetail
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode user/get response: %w", err)
	}

	if result.ErrCode != 0 {
		return nil, fmt.Errorf("wecom user/get failed [%d]: %s", result.ErrCode, result.ErrMsg)
	}

	return &result.UserDetail, nil
}

func (c *Client) UpdateCorpID(corpID string) {
	c.appTokenLock.Lock()
	defer c.appTokenLock.Unlock()
	c.cfg.CorpID = corpID
}
