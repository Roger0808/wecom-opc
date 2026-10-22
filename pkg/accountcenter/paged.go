package accountcenter

import (
	"context"
	"errors"
	"fmt"
)

// ErrSnapshotIncomplete 表示分页拉取结果与账户中心聚合计数对账不一致
// （含拉取为空但聚合计数非零的快照）。附计数信息。
var ErrSnapshotIncomplete = errors.New("account center snapshot incomplete")

const defaultSnapshotPageSize = 500

// maxConnectionPages 单连接分页拉取的页数上限（防御上游分页协议异常导致的死循环）。
// var 而非 const：测试可临时调小以覆盖上限路径。
var maxConnectionPages = 2000

const sysDepartmentNodeSelection = `
	id
	parentCode
	departmentCode
	departmentName
	departmentSort
	departmentStatus
	departmentLevel
	thirdDepartmentId
	thirdManagerId
`

const sysUserNodeSelection = `
	id
	userNo
	userName
	employeeId
	mobile
	email
	departmentCode
	userType
	userStatus
	syncFrom
	unionNo
	thirdUserId
	isDeleted
	createTime
	updateTime
`

type connectionPage[T any] struct {
	PageInfo *struct {
		HasNextPage *bool   `json:"hasNextPage"`
		EndCursor   *string `json:"endCursor"`
	} `json:"pageInfo"`
	Edges []struct {
		Node T `json:"node"`
	} `json:"edges"`
}

// buildConnectionQuery 生成 nestjs-query 风格的分页连接查询。
// field 为根字段名（如 sysUsers），filterType/sortType 为对应输入类型名，
// nodeSelection 为 edges.node 内的字段选择。
func buildConnectionQuery(field, filterType, sortType, nodeSelection string) string {
	return fmt.Sprintf(`
		query FetchConnection($filter: %s!, $paging: CursorPaging!, $sorting: [%s!]) {
			%s(filter: $filter, paging: $paging, sorting: $sorting) {
				pageInfo {
					hasNextPage
					endCursor
				}
				edges {
					node {
						%s
					}
				}
			}
		}
	`, filterType, sortType, field, nodeSelection)
}

// iterateConnection 以游标分页循环拉取连接的全部节点，直到 hasNextPage 为 false。
// 防御（L9）：页数超过 maxConnectionPages 或游标停滞（连续两页 endCursor 相同）
// 即判定分页协议异常，返回 ErrSnapshotIncomplete（映射 SYNC_SNAPSHOT_INCOMPLETE）。
func iterateConnection[T any](ctx context.Context, c *Client, query, field string, filter map[string]interface{}, sorting []map[string]interface{}, first int) ([]T, error) {
	if first <= 0 {
		first = defaultSnapshotPageSize
	}

	var all []T
	after := ""
	for page := 1; ; page++ {
		if page > maxConnectionPages {
			return nil, fmt.Errorf("%w: %s page limit exceeded (%d pages)", ErrSnapshotIncomplete, field, maxConnectionPages)
		}
		paging := map[string]interface{}{"first": first}
		if after != "" {
			paging["after"] = after
		}
		variables := map[string]interface{}{
			"filter":  filter,
			"paging":  paging,
			"sorting": sorting,
		}

		var resp map[string]connectionPage[T]
		if err := c.query(ctx, query, variables, &resp); err != nil {
			return nil, err
		}

		page, ok := resp[field]
		if !ok {
			return nil, fmt.Errorf("graphql response missing %q", field)
		}
		if page.PageInfo == nil || page.PageInfo.HasNextPage == nil {
			return nil, fmt.Errorf("graphql response %q missing pageInfo.hasNextPage", field)
		}

		for _, edge := range page.Edges {
			all = append(all, edge.Node)
		}

		if !*page.PageInfo.HasNextPage {
			break
		}
		if page.PageInfo.EndCursor == nil || *page.PageInfo.EndCursor == "" {
			return nil, fmt.Errorf("graphql response %q missing pageInfo.endCursor while hasNextPage is true", field)
		}
		if *page.PageInfo.EndCursor == after {
			return nil, fmt.Errorf("%w: %s cursor stagnated at %q", ErrSnapshotIncomplete, field, after)
		}
		after = *page.PageInfo.EndCursor
	}

	return all, nil
}

// aggregateCount 调用 nestjs-query 聚合端点，返回 count { id } 的总数。
func (c *Client) aggregateCount(ctx context.Context, field, filterType string, filter map[string]interface{}) (int, error) {
	query := fmt.Sprintf(`
		query AggregateCount($filter: %s!) {
			%s(filter: $filter) {
				count {
					id
				}
			}
		}
	`, filterType, field)

	var resp map[string][]struct {
		Count struct {
			ID int `json:"id"`
		} `json:"count"`
	}
	if err := c.query(ctx, query, map[string]interface{}{"filter": filter}, &resp); err != nil {
		return 0, err
	}

	groups := resp[field]
	if len(groups) == 0 {
		return 0, fmt.Errorf("graphql response %q missing aggregate group", field)
	}
	return groups[0].Count.ID, nil
}

func activeDepartmentFilter() map[string]interface{} {
	return map[string]interface{}{
		"departmentStatus": map[string]interface{}{"eq": 1},
		"isDeleted":        map[string]interface{}{"eq": 0},
	}
}

func activeUserFilter(departmentCode string) map[string]interface{} {
	filter := map[string]interface{}{
		"userStatus": map[string]interface{}{"eq": 1},
		"isDeleted":  map[string]interface{}{"eq": 0},
	}
	if departmentCode != "" {
		filter["departmentCode"] = map[string]interface{}{"eq": departmentCode}
	}
	return filter
}

// FetchActiveDepartmentsPaged 分页拉取全部启用且未删除的部门，
// 结束时与 sysDepartmentAggregate 同过滤计数对账，不一致返回 ErrSnapshotIncomplete。
func (c *Client) FetchActiveDepartmentsPaged(ctx context.Context) ([]SysDepartment, error) {
	field := "sysDepartments"
	query := buildConnectionQuery(field, "SysDepartmentFilter", "SysDepartmentSort", sysDepartmentNodeSelection)
	sorting := []map[string]interface{}{{"field": "id", "direction": "ASC"}}

	list, err := iterateConnection[SysDepartment](ctx, c, query, field, activeDepartmentFilter(), sorting, c.pageSize)
	if err != nil {
		return nil, err
	}

	total, err := c.aggregateCount(ctx, "sysDepartmentAggregate", "SysDepartmentAggregateFilter", activeDepartmentFilter())
	if err != nil {
		return nil, err
	}
	if len(list) != total {
		return nil, fmt.Errorf("%w: departments fetched=%d aggregate=%d", ErrSnapshotIncomplete, len(list), total)
	}
	return list, nil
}

// FetchActiveUsersPaged 分页拉取在职（userStatus==1 且 isDeleted==0）用户，
// departmentCode 非空时仅拉取该部门。结束时与 sysUserAggregate 同过滤计数对账，
// 不一致返回 ErrSnapshotIncomplete。
func (c *Client) FetchActiveUsersPaged(ctx context.Context, departmentCode string) ([]SysUser, error) {
	field := "sysUsers"
	query := buildConnectionQuery(field, "SysUserFilter", "SysUserSort", sysUserNodeSelection)
	sorting := []map[string]interface{}{{"field": "id", "direction": "ASC"}}
	filter := activeUserFilter(departmentCode)

	list, err := iterateConnection[SysUser](ctx, c, query, field, filter, sorting, c.pageSize)
	if err != nil {
		return nil, err
	}

	total, err := c.aggregateCount(ctx, "sysUserAggregate", "SysUserAggregateFilter", filter)
	if err != nil {
		return nil, err
	}
	if len(list) != total {
		return nil, fmt.Errorf("%w: users fetched=%d aggregate=%d", ErrSnapshotIncomplete, len(list), total)
	}
	return list, nil
}

// CountActiveUsers 返回在职（userStatus==1 且 isDeleted==0）用户的聚合计数，
// departmentCode 非空时仅统计该部门。供完整性预检使用。
func (c *Client) CountActiveUsers(ctx context.Context, departmentCode string) (int, error) {
	return c.aggregateCount(ctx, "sysUserAggregate", "SysUserFilter", activeUserFilter(departmentCode))
}
