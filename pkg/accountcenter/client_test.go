package accountcenter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

type capturedRequest struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables"`
}

// fakeGraphQLServer 返回一个记录所有请求并按 handler 生成响应的假 GraphQL server。
func fakeGraphQLServer(t *testing.T, handler func(req capturedRequest) map[string]interface{}) (*httptest.Server, *[]capturedRequest) {
	t.Helper()
	var mu sync.Mutex
	var captured []capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req capturedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		captured = append(captured, req)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": handler(req)})
	}))
	t.Cleanup(srv.Close)
	return srv, &captured
}

func pagingVars(t *testing.T, req capturedRequest) (first float64, after string, hasAfter bool) {
	t.Helper()
	paging, ok := req.Variables["paging"].(map[string]interface{})
	if !ok {
		t.Fatalf("paging variables missing: %+v", req.Variables)
	}
	first, _ = paging["first"].(float64)
	after, hasAfter = paging["after"].(string)
	return
}

func TestFetchActiveDepartmentsPaged_MultiPage(t *testing.T) {
	var departments []map[string]interface{}
	for i := 1; i <= 7; i++ {
		departments = append(departments, map[string]interface{}{
			"id":               fmt.Sprintf("%d", i),
			"departmentCode":   fmt.Sprintf("D%03d", i),
			"departmentName":   fmt.Sprintf("部门%d", i),
			"departmentStatus": 1,
			"departmentLevel":  1,
		})
	}

	srv, captured := fakeGraphQLServer(t, func(req capturedRequest) map[string]interface{} {
		if _, isPaging := req.Variables["paging"].(map[string]interface{}); !isPaging {
			return map[string]interface{}{
				"sysDepartmentAggregate": []map[string]interface{}{
					{"count": map[string]interface{}{"id": len(departments)}},
				},
			}
		}
		_, after, _ := pagingVars(t, req)
		pageIdx := 0
		switch after {
		case "":
			pageIdx = 0
		case "cursor-1":
			pageIdx = 1
		case "cursor-2":
			pageIdx = 2
		default:
			t.Errorf("unexpected after cursor: %q", after)
		}
		start := pageIdx * 3
		end := start + 3
		hasNext := end < len(departments)
		if !hasNext {
			end = len(departments)
		}
		var edges []map[string]interface{}
		for _, d := range departments[start:end] {
			edges = append(edges, map[string]interface{}{"node": d})
		}
		endCursor := ""
		if hasNext {
			endCursor = fmt.Sprintf("cursor-%d", pageIdx+1)
		}
		return map[string]interface{}{
			"sysDepartments": map[string]interface{}{
				"pageInfo": map[string]interface{}{"hasNextPage": hasNext, "endCursor": endCursor},
				"edges":    edges,
			},
		}
	})

	c := NewClient(srv.URL)
	c.pageSize = 3

	got, err := c.FetchActiveDepartmentsPaged(context.Background())
	if err != nil {
		t.Fatalf("FetchActiveDepartmentsPaged: %v", err)
	}
	if len(got) != 7 {
		t.Fatalf("expected 7 departments, got %d", len(got))
	}
	for i, d := range got {
		if d.ID != fmt.Sprintf("%d", i+1) {
			t.Errorf("department[%d].ID = %q, want %q", i, d.ID, fmt.Sprintf("%d", i+1))
		}
	}

	// 3 页循环 + 1 次聚合对账
	if len(*captured) != 4 {
		t.Fatalf("expected 4 requests (3 pages + 1 aggregate), got %d", len(*captured))
	}
	for i, req := range (*captured)[:3] {
		first, after, hasAfter := pagingVars(t, req)
		if first != 3 {
			t.Errorf("page %d: first = %v, want 3", i, first)
		}
		if (i == 0 && hasAfter) || (i > 0 && (!hasAfter || after != fmt.Sprintf("cursor-%d", i))) {
			t.Errorf("page %d: after = %q (present=%v)", i, after, hasAfter)
		}
		filter, ok := req.Variables["filter"].(map[string]interface{})
		if !ok {
			t.Fatalf("page %d: filter missing", i)
		}
		if filter["departmentStatus"].(map[string]interface{})["eq"] != float64(1) ||
			filter["isDeleted"].(map[string]interface{})["eq"] != float64(0) {
			t.Errorf("page %d: wrong filter: %+v", i, filter)
		}
		sorting, ok := req.Variables["sorting"].([]interface{})
		if !ok || len(sorting) != 1 {
			t.Fatalf("page %d: sorting missing", i)
		}
		s0 := sorting[0].(map[string]interface{})
		if s0["field"] != "id" || s0["direction"] != "ASC" {
			t.Errorf("page %d: wrong sorting: %+v", i, sorting)
		}
	}
}

func TestFetchActiveUsersPaged_AggregateMismatch(t *testing.T) {
	srv, _ := fakeGraphQLServer(t, func(req capturedRequest) map[string]interface{} {
		if _, isPaging := req.Variables["paging"].(map[string]interface{}); !isPaging {
			return map[string]interface{}{
				"sysUserAggregate": []map[string]interface{}{
					{"count": map[string]interface{}{"id": 3}},
				},
			}
		}
		return map[string]interface{}{
			"sysUsers": map[string]interface{}{
				"pageInfo": map[string]interface{}{"hasNextPage": false, "endCursor": nil},
				"edges": []map[string]interface{}{
					{"node": map[string]interface{}{"id": "1", "userNo": "U1"}},
					{"node": map[string]interface{}{"id": "2", "userNo": "U2"}},
				},
			},
		}
	})

	c := NewClient(srv.URL)
	c.pageSize = 10

	_, err := c.FetchActiveUsersPaged(context.Background(), "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrSnapshotIncomplete) {
		t.Fatalf("expected ErrSnapshotIncomplete, got %v", err)
	}
}

func TestFetchActiveUsersPaged_Empty(t *testing.T) {
	srv, captured := fakeGraphQLServer(t, func(req capturedRequest) map[string]interface{} {
		if _, isPaging := req.Variables["paging"].(map[string]interface{}); !isPaging {
			return map[string]interface{}{
				"sysUserAggregate": []map[string]interface{}{
					{"count": map[string]interface{}{"id": 0}},
				},
			}
		}
		return map[string]interface{}{
			"sysUsers": map[string]interface{}{
				"pageInfo": map[string]interface{}{"hasNextPage": false, "endCursor": nil},
				"edges":    []map[string]interface{}{},
			},
		}
	})

	c := NewClient(srv.URL)
	got, err := c.FetchActiveUsersPaged(context.Background(), "D001")
	if err != nil {
		t.Fatalf("FetchActiveUsersPaged: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty result, got %d users", len(got))
	}

	// filter 必须携带 departmentCode，且不传 after 时 paging 不得有 after
	pageReq := (*captured)[0]
	filter := pageReq.Variables["filter"].(map[string]interface{})
	if filter["departmentCode"].(map[string]interface{})["eq"] != "D001" {
		t.Errorf("filter missing departmentCode eq D001: %+v", filter)
	}
	if filter["userStatus"].(map[string]interface{})["eq"] != float64(1) ||
		filter["isDeleted"].(map[string]interface{})["eq"] != float64(0) {
		t.Errorf("wrong active-user filter: %+v", filter)
	}
	paging := pageReq.Variables["paging"].(map[string]interface{})
	if _, hasAfter := paging["after"]; hasAfter {
		t.Errorf("first page must not carry after: %+v", paging)
	}

	// 聚合对账使用同一 filter
	aggReq := (*captured)[1]
	aggFilter := aggReq.Variables["filter"].(map[string]interface{})
	if aggFilter["departmentCode"].(map[string]interface{})["eq"] != "D001" {
		t.Errorf("aggregate filter missing departmentCode: %+v", aggFilter)
	}
}

func TestFetchActiveUsersPaged_AllScopeOmitsDepartmentCode(t *testing.T) {
	srv, captured := fakeGraphQLServer(t, func(req capturedRequest) map[string]interface{} {
		if _, isPaging := req.Variables["paging"].(map[string]interface{}); !isPaging {
			return map[string]interface{}{
				"sysUserAggregate": []map[string]interface{}{
					{"count": map[string]interface{}{"id": 1}},
				},
			}
		}
		return map[string]interface{}{
			"sysUsers": map[string]interface{}{
				"pageInfo": map[string]interface{}{"hasNextPage": false, "endCursor": nil},
				"edges": []map[string]interface{}{
					{"node": map[string]interface{}{"id": "1", "userStatus": 1}},
				},
			},
		}
	})

	c := NewClient(srv.URL)
	got, err := c.FetchActiveUsersPaged(context.Background(), "")
	if err != nil {
		t.Fatalf("FetchActiveUsersPaged: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 user, got %d", len(got))
	}
	filter := (*captured)[0].Variables["filter"].(map[string]interface{})
	if _, hasDept := filter["departmentCode"]; hasDept {
		t.Errorf("all-scope filter must not contain departmentCode: %+v", filter)
	}
}

func TestFetchActiveUsersPaged_PageInfoMissingHasNextPage(t *testing.T) {
	srv, _ := fakeGraphQLServer(t, func(req capturedRequest) map[string]interface{} {
		return map[string]interface{}{
			"sysUsers": map[string]interface{}{
				"pageInfo": map[string]interface{}{"endCursor": "cursor-1"},
				"edges": []map[string]interface{}{
					{"node": map[string]interface{}{"id": "1"}},
				},
			},
		}
	})

	c := NewClient(srv.URL)
	_, err := c.FetchActiveUsersPaged(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for missing hasNextPage, got nil")
	}
	if errors.Is(err, ErrSnapshotIncomplete) {
		t.Fatalf("missing pageInfo should not be classified as snapshot incomplete: %v", err)
	}
}

func TestFetchActiveDepartmentsPaged_MissingEndCursor(t *testing.T) {
	srv, _ := fakeGraphQLServer(t, func(req capturedRequest) map[string]interface{} {
		return map[string]interface{}{
			"sysDepartments": map[string]interface{}{
				"pageInfo": map[string]interface{}{"hasNextPage": true, "endCursor": nil},
				"edges": []map[string]interface{}{
					{"node": map[string]interface{}{"id": "1"}},
				},
			},
		}
	})

	c := NewClient(srv.URL)
	_, err := c.FetchActiveDepartmentsPaged(context.Background())
	if err == nil {
		t.Fatal("expected error for missing endCursor with hasNextPage=true, got nil")
	}
}

func TestCountActiveUsers(t *testing.T) {
	srv, captured := fakeGraphQLServer(t, func(req capturedRequest) map[string]interface{} {
		return map[string]interface{}{
			"sysUserAggregate": []map[string]interface{}{
				{"count": map[string]interface{}{"id": 42}},
			},
		}
	})

	c := NewClient(srv.URL)
	n, err := c.CountActiveUsers(context.Background(), "D007")
	if err != nil {
		t.Fatalf("CountActiveUsers: %v", err)
	}
	if n != 42 {
		t.Fatalf("CountActiveUsers = %d, want 42", n)
	}
	if len(*captured) != 1 {
		t.Fatalf("expected 1 aggregate request, got %d", len(*captured))
	}
	filter := (*captured)[0].Variables["filter"].(map[string]interface{})
	if filter["departmentCode"].(map[string]interface{})["eq"] != "D007" {
		t.Errorf("filter missing departmentCode eq D007: %+v", filter)
	}
}

func TestGetUserByUnionNo_Regression(t *testing.T) {
	srv, captured := fakeGraphQLServer(t, func(req capturedRequest) map[string]interface{} {
		return map[string]interface{}{
			"sysUsers": map[string]interface{}{
				"edges": []map[string]interface{}{
					{"node": map[string]interface{}{
						"id":         "9",
						"userNo":     "U9",
						"userName":   "张三",
						"unionNo":    "union-9",
						"userStatus": 1,
					}},
				},
			},
		}
	})

	c := NewClient(srv.URL)
	user, err := c.GetUserByUnionNo(context.Background(), "union-9")
	if err != nil {
		t.Fatalf("GetUserByUnionNo: %v", err)
	}
	if user == nil || user.ID != "9" || user.UnionNo != "union-9" {
		t.Fatalf("unexpected user: %+v", user)
	}
	vars := (*captured)[0].Variables
	if vars["unionNo"] != "union-9" {
		t.Errorf("variables unionNo = %v", vars["unionNo"])
	}
}

func TestGetUserByThirdUserID_Regression(t *testing.T) {
	srv, captured := fakeGraphQLServer(t, func(req capturedRequest) map[string]interface{} {
		return map[string]interface{}{
			"sysUsers": map[string]interface{}{
				"edges": []map[string]interface{}{
					{"node": map[string]interface{}{
						"id":          "5",
						"thirdUserId": "third-5",
					}},
				},
			},
		}
	})

	c := NewClient(srv.URL)
	user, err := c.GetUserByThirdUserID(context.Background(), "third-5")
	if err != nil {
		t.Fatalf("GetUserByThirdUserID: %v", err)
	}
	if user == nil || user.ThirdUserID != "third-5" {
		t.Fatalf("unexpected user: %+v", user)
	}
	vars := (*captured)[0].Variables
	if vars["thirdUserId"] != "third-5" {
		t.Errorf("variables thirdUserId = %v", vars["thirdUserId"])
	}

	if empty, err := c.GetUserByThirdUserID(context.Background(), ""); err != nil || empty != nil {
		t.Fatalf("empty thirdUserID should return nil,nil, got %+v, %v", empty, err)
	}
	if len(*captured) != 1 {
		t.Fatalf("empty thirdUserID must not hit network, got %d requests", len(*captured))
	}
}

func TestGraphQLError_NotLeakingStack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"errors": []map[string]interface{}{
				{"message": "select * from sys_user where ..."},
			},
		})
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL)
	_, err := c.CountActiveUsers(context.Background(), "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	if len(msg) > 200 {
		t.Fatalf("error message suspiciously long (possible leak): %d chars", len(msg))
	}
}
