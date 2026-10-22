package accountcenter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Client struct {
	endpoint   string
	httpClient *http.Client
	pageSize   int
}

func NewClient(endpoint string) *Client {
	if endpoint == "" {
		endpoint = "http://omni-router.benlai.com/graphql"
	}
	return &Client{
		endpoint: endpoint,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
		pageSize: defaultSnapshotPageSize,
	}
}

// SysDepartment 部门实体
type SysDepartment struct {
	ID                string `json:"id"`
	ParentCode        string `json:"parentCode"`
	DepartmentCode    string `json:"departmentCode"`
	DepartmentName    string `json:"departmentName"`
	DepartmentSort    int    `json:"departmentSort"`
	DepartmentStatus  int    `json:"departmentStatus"`
	DepartmentLevel   int    `json:"departmentLevel"`
	ThirdDepartmentID string `json:"thirdDepartmentId"`
	ThirdManagerID    string `json:"thirdManagerId"`
}

// SysUser 用户实体
type SysUser struct {
	ID             string `json:"id"`
	UserNo         string `json:"userNo"`
	UserName       string `json:"userName"`
	EmployeeID     string `json:"employeeId"`
	Mobile         string `json:"mobile"`
	Email          string `json:"email"`
	DepartmentCode string `json:"departmentCode"`
	UserType       int    `json:"userType"`
	UserStatus     int    `json:"userStatus"`
	SyncFrom       int    `json:"syncFrom"`
	UnionNo        string `json:"unionNo"` // 对应企业微信 UserID
	ThirdUserID    string `json:"thirdUserId"`
	IsDeleted      int    `json:"isDeleted"`
	CreateTime     string `json:"createTime"`
	UpdateTime     string `json:"updateTime"`
}

type graphQLRequest struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables,omitempty"`
}

type graphQLResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func (c *Client) query(ctx context.Context, query string, variables map[string]interface{}, out interface{}) error {
	reqBody, err := json.Marshal(graphQLRequest{
		Query:     query,
		Variables: variables,
	})
	if err != nil {
		return fmt.Errorf("marshal graphql request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("create http request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("graphql request failed: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("graphql http %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var gqlResp graphQLResponse
	if err := json.Unmarshal(bodyBytes, &gqlResp); err != nil {
		return fmt.Errorf("unmarshal graphql response: %w", err)
	}

	if len(gqlResp.Errors) > 0 {
		return fmt.Errorf("graphql error: %s", gqlResp.Errors[0].Message)
	}

	if err := json.Unmarshal(gqlResp.Data, out); err != nil {
		return fmt.Errorf("unmarshal data field: %w", err)
	}

	return nil
}

// FetchAllActiveDepartments 拉取所有未删除且启用的部门
func (c *Client) FetchAllActiveDepartments(ctx context.Context) ([]SysDepartment, error) {
	query := `
		query GetAllDepartments {
			sysDepartments(
				filter: { isDeleted: { eq: 0 }, departmentStatus: { eq: 1 } }
				paging: { first: 2000 }
				sorting: [
					{ field: departmentLevel, direction: ASC }
					{ field: departmentSort, direction: ASC }
				]
			) {
				edges {
					node {
						id
						parentCode
						departmentCode
						departmentName
						departmentSort
						departmentStatus
						departmentLevel
						thirdDepartmentId
						thirdManagerId
					}
				}
			}
		}
	`

	var resp struct {
		SysDepartments struct {
			Edges []struct {
				Node SysDepartment `json:"node"`
			} `json:"edges"`
		} `json:"sysDepartments"`
	}

	if err := c.query(ctx, query, nil, &resp); err != nil {
		return nil, err
	}

	list := make([]SysDepartment, len(resp.SysDepartments.Edges))
	for i, edge := range resp.SysDepartments.Edges {
		list[i] = edge.Node
	}
	return list, nil
}

// GetUserByUnionNo 根据企微 UserID (unionNo) 查询员工
func (c *Client) GetUserByUnionNo(ctx context.Context, unionNo string) (*SysUser, error) {
	query := `
		query GetUserByUnionNo($unionNo: String!) {
			sysUsers(
				filter: { unionNo: { eq: $unionNo }, isDeleted: { eq: 0 } }
				paging: { first: 1 }
			) {
				edges {
					node {
						id
						userNo
						userName
						employeeId
						mobile
						email
						departmentCode
						userStatus
						unionNo
						thirdUserId
					}
				}
			}
		}
	`

	variables := map[string]interface{}{
		"unionNo": unionNo,
	}

	var resp struct {
		SysUsers struct {
			Edges []struct {
				Node SysUser `json:"node"`
			} `json:"edges"`
		} `json:"sysUsers"`
	}

	if err := c.query(ctx, query, variables, &resp); err != nil {
		return nil, err
	}

	if len(resp.SysUsers.Edges) == 0 {
		return nil, nil
	}

	return &resp.SysUsers.Edges[0].Node, nil
}

// GetUserByThirdUserID 根据 thirdUserId 查询用户 (用于找主管)
func (c *Client) GetUserByThirdUserID(ctx context.Context, thirdUserID string) (*SysUser, error) {
	if thirdUserID == "" {
		return nil, nil
	}

	query := `
		query GetUserByThirdUserId($thirdUserId: String!) {
			sysUsers(
				filter: { thirdUserId: { eq: $thirdUserId }, isDeleted: { eq: 0 } }
				paging: { first: 1 }
			) {
				edges {
					node {
						id
						userNo
						userName
						employeeId
						mobile
						email
						departmentCode
						userStatus
						unionNo
						thirdUserId
					}
				}
			}
		}
	`

	variables := map[string]interface{}{
		"thirdUserId": thirdUserID,
	}

	var resp struct {
		SysUsers struct {
			Edges []struct {
				Node SysUser `json:"node"`
			} `json:"edges"`
		} `json:"sysUsers"`
	}

	if err := c.query(ctx, query, variables, &resp); err != nil {
		return nil, err
	}

	if len(resp.SysUsers.Edges) == 0 {
		return nil, nil
	}

	return &resp.SysUsers.Edges[0].Node, nil
}
