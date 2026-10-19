package syncer

import (
	"context"
	"testing"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/accountcenter"
)

func TestFindTargetDepartmentForUserLevel1(t *testing.T) {
	// 构造测试部门数据：
	// Level 1: 技术中心 (T01)
	// Level 2: 架构部 (T02, parent: T01)
	// Level 3: AI组 (T03, parent: T02)
	// Level 1: 财务部 (F01)
	allDepts := []accountcenter.SysDepartment{
		{ID: "1", DepartmentCode: "T01", DepartmentName: "技术中心", DepartmentLevel: 1, ParentCode: ""},
		{ID: "2", DepartmentCode: "T02", DepartmentName: "架构部", DepartmentLevel: 2, ParentCode: "T01"},
		{ID: "3", DepartmentCode: "T03", DepartmentName: "AI组", DepartmentLevel: 3, ParentCode: "T02"},
		{ID: "4", DepartmentCode: "F01", DepartmentName: "财务部", DepartmentLevel: 1, ParentCode: ""},
		{ID: "5", DepartmentCode: "F02", DepartmentName: "结算组", DepartmentLevel: 2, ParentCode: "F01"},
	}

	deptMap := make(map[string]accountcenter.SysDepartment)
	targetMap := make(map[string]accountcenter.SysDepartment)

	for _, d := range allDepts {
		deptMap[d.DepartmentCode] = d
		// 只同步 level == 1 的部门作为目标空间
		if d.DepartmentLevel == 1 {
			targetMap[d.DepartmentCode] = d
		}
	}

	syncer := &Syncer{
		cfg:         &config.Config{},
		deptMap:     deptMap,
		targetDepts: targetMap,
	}

	ctx := context.Background()

	// 1. 测试 Level 3 员工追溯到 Level 1
	deptL3, err := syncer.FindTargetDepartmentForUser(ctx, "T03")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deptL3 == nil || deptL3.DepartmentCode != "T01" {
		t.Errorf("expected target dept T01 (技术中心), got %+v", deptL3)
	}

	// 2. 测试 Level 2 员工追溯到 Level 1
	deptL2, err := syncer.FindTargetDepartmentForUser(ctx, "T02")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deptL2 == nil || deptL2.DepartmentCode != "T01" {
		t.Errorf("expected target dept T01 (技术中心), got %+v", deptL2)
	}

	// 3. 测试 Level 1 员工直接命中
	deptL1, err := syncer.FindTargetDepartmentForUser(ctx, "T01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deptL1 == nil || deptL1.DepartmentCode != "T01" {
		t.Errorf("expected target dept T01 (技术中心), got %+v", deptL1)
	}

	// 4. 测试另一个分支财务部
	deptF2, err := syncer.FindTargetDepartmentForUser(ctx, "F02")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deptF2 == nil || deptF2.DepartmentCode != "F01" {
		t.Errorf("expected target dept F01 (财务部), got %+v", deptF2)
	}

	// 5. 测试未知部门
	deptUnknown, err := syncer.FindTargetDepartmentForUser(ctx, "UNKNOWN")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deptUnknown != nil {
		t.Errorf("expected nil for unknown dept, got %+v", deptUnknown)
	}
}
