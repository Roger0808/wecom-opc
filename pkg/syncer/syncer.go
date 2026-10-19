package syncer

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/accountcenter"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"
	"weknora-tokendance/pkg/wecom"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

type Syncer struct {
	cfg     *config.Config
	wecom   *wecom.Client
	ac      *accountcenter.Client
	storage *storage.Storage

	deptCacheLock sync.RWMutex
	deptMap       map[string]accountcenter.SysDepartment // departmentCode -> SysDepartment
	targetDepts   map[string]accountcenter.SysDepartment // departmentCode -> SysDepartment (已同步到 WeKnora 的空间部门)
}

func NewSyncer(cfg *config.Config, wecomClient *wecom.Client, acClient *accountcenter.Client, storage *storage.Storage) *Syncer {
	return &Syncer{
		cfg:         cfg,
		wecom:       wecomClient,
		ac:          acClient,
		storage:     storage,
		deptMap:     make(map[string]accountcenter.SysDepartment),
		targetDepts: make(map[string]accountcenter.SysDepartment),
	}
}

// SyncAll 执行全量同步：
// 1. 从统一账户中心拉取所有部门并构建层级树；
// 2. 筛选出所有 level == 1 的一级部门作为租户空间；
// 3. 将这些部门同步并在 WeKnora 创建对应 Tenant；
// 4. 按父共享空间的 LLM Provider 所有的配置，同步到相应的部门 LLM Provider 配置中；
// 5. 将部门主管自动关联为空间管理员 (Admin)。
func (s *Syncer) SyncAll(ctx context.Context) error {
	log.Println("[Syncer] 开始从统一账户中心拉取组织架构...")

	allDepts, err := s.ac.FetchAllActiveDepartments(ctx)
	if err != nil {
		return fmt.Errorf("fetch departments from account center: %w", err)
	}
	log.Printf("[Syncer] 成功获取统一账户中心有效部门 %d 个", len(allDepts))

	// 构建部门映射表
	deptMap := make(map[string]accountcenter.SysDepartment, len(allDepts))
	for _, d := range allDepts {
		deptMap[d.DepartmentCode] = d
	}

	// 筛选目标空间部门：只保留 level == 1 的一级部门
	var targetList []accountcenter.SysDepartment
	targetMap := make(map[string]accountcenter.SysDepartment)

	for _, d := range allDepts {
		if d.DepartmentLevel == 1 {
			targetList = append(targetList, d)
			targetMap[d.DepartmentCode] = d
		}
	}

	s.deptCacheLock.Lock()
	s.deptMap = deptMap
	s.targetDepts = targetMap
	s.deptCacheLock.Unlock()

	log.Printf("[Syncer] 筛选出符合规则的租户空间部门 %d 个 (仅 level==1 一级部门)", len(targetList))

	// 遍历并同步至 WeKnora 空间
	for _, dept := range targetList {
		if err := s.syncDepartmentToTenant(ctx, dept, deptMap); err != nil {
			log.Printf("[Syncer] 部门同步失败 [%s: %s]: %v", dept.DepartmentCode, dept.DepartmentName, err)
		}
	}

	log.Println("[Syncer] 组织架构空间同步完成！")
	return nil
}

// buildDepartmentHierarchyPath 构建部门层级全路径，如: "运营与技术本部 / 技术交付中心 / BDE平台开发部"
func (s *Syncer) buildDepartmentHierarchyPath(d accountcenter.SysDepartment, deptMap map[string]accountcenter.SysDepartment) string {
	names := []string{}
	curr := d
	for {
		// 跳过顶级集团名称，保持简洁
		if curr.DepartmentLevel >= 0 {
			names = append([]string{curr.DepartmentName}, names...)
		}
		if curr.ParentCode == "" {
			break
		}
		parent, ok := deptMap[curr.ParentCode]
		if !ok {
			break
		}
		curr = parent
	}
	if len(names) == 0 {
		return d.DepartmentName
	}
	return strings.Join(names, " / ")
}

// syncDepartmentToTenant 将统一账户中心部门持久化为 WeKnora Tenant
func (s *Syncer) syncDepartmentToTenant(ctx context.Context, dept accountcenter.SysDepartment, deptMap map[string]accountcenter.SysDepartment) error {
	deptIntID, _ := strconv.ParseInt(dept.ID, 10, 64)
	if deptIntID == 0 {
		deptIntID = 1
	}

	// 1. 查找是否已经有映射
	mapping, err := s.storage.FindTenantByDeptCode(ctx, dept.DepartmentCode)
	if err != nil || mapping == nil {
		mapping, _ = s.storage.FindTenantByWeComDeptID(ctx, deptIntID)
	}

	var tenantID uint64
	if mapping != nil {
		tenantID = mapping.TenantID
	} else {
		// 生成租户 ID: 20000 + deptIntID
		tenantID = uint64(20000 + deptIntID)
	}

	hierarchyPath := s.buildDepartmentHierarchyPath(dept, deptMap)

	// 2. 确保 WeKnora tenants 表有对应记录
	tenant := &model.WeKnoraTenant{
		ID:               tenantID,
		Name:             dept.DepartmentName,
		Description:      fmt.Sprintf("层级: L%d | 路径: %s (编码: %s)", dept.DepartmentLevel, hierarchyPath, dept.DepartmentCode),
		Status:           "active",
		RetrieverEngines: `{"engines":[]}`,
		StorageQuota:     10 * 1024 * 1024 * 1024, // 默认 10GB
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}

	if err := s.storage.CreateTenantIfNotExists(ctx, tenant); err != nil {
		return fmt.Errorf("create tenant in db: %w", err)
	}

	// 3. 保存/更新映射表 (记录 department_code 与 we_com_dept_id)
	parentIntID, _ := strconv.ParseInt(dept.ParentCode, 10, 64)
	err = s.storage.SaveDeptTenantMapping(ctx, &model.WeComDeptTenantMapping{
		WeComDeptID:    deptIntID,
		DepartmentCode: dept.DepartmentCode,
		TenantID:       tenantID,
		DeptName:       dept.DepartmentName,
		ParentID:       parentIntID,
		UpdatedAt:      time.Now(),
	})
	if err != nil {
		return fmt.Errorf("save mapping in db: %w", err)
	}

	// 4. 按父共享空间的 LLM Provider 所有的配置，同步到当前部门空间中
	if s.cfg.WeKnora.RootTenantID > 0 && s.cfg.WeKnora.RootTenantID != tenantID {
		if err := s.storage.SyncTenantModelsFromParent(ctx, s.cfg.WeKnora.RootTenantID, tenantID); err != nil {
			log.Printf("[Syncer] 同步父空间 LLM 配置至部门空间失败 [%s -> tenant %d]: %v", dept.DepartmentName, tenantID, err)
		} else {
			log.Printf("[Syncer] 成功同步父空间 LLM 配置至部门空间 [%s -> tenant %d]", dept.DepartmentName, tenantID)
		}
	}

	// 5. 确保存储后端 (MinIO + Local) 配置就绪
	if s.cfg.WeKnora.RootTenantID > 0 {
		if err := s.storage.SyncSpaceStorage(ctx, tenantID, s.cfg.WeKnora.RootTenantID); err != nil {
			log.Printf("[Syncer] 同步空间存储配置失败 [%s -> tenant %d]: %v", dept.DepartmentName, tenantID, err)
		} else {
			log.Printf("[Syncer] 成功同步空间存储配置 [%s -> tenant %d]", dept.DepartmentName, tenantID)
		}
	}

	// 5.1 确保沙箱环境 (Docker Sandbox) 配置就绪
	if s.cfg.WeKnora.RootTenantID > 0 {
		if err := s.storage.SyncSpaceSandbox(ctx, tenantID, s.cfg.WeKnora.RootTenantID); err != nil {
			log.Printf("[Syncer] 同步空间沙箱配置失败 [%s -> tenant %d]: %v", dept.DepartmentName, tenantID, err)
		} else {
			log.Printf("[Syncer] 成功同步空间沙箱配置 [%s -> tenant %d]", dept.DepartmentName, tenantID)
		}
	}

	// 6. 若部门存在主管 (thirdManagerId)，自动预置为主管并赋予空间 Admin 角色
	if dept.ThirdManagerID != "" {
		go func(managerThirdID string, targetTenantID uint64) {
			mUser, mErr := s.ac.GetUserByThirdUserID(context.Background(), managerThirdID)
			if mErr == nil && mUser != nil {
				_ = s.SyncUserAndMemberships(context.Background(), mUser, &dept, true)
			}
		}(dept.ThirdManagerID, tenantID)
	}

	return nil
}

// SyncSingleSpace 针对指定空间同步模型、存储引擎与沙箱环境配置
func (s *Syncer) SyncSingleSpace(ctx context.Context, tenantID uint64) error {
	if s.cfg.WeKnora.RootTenantID > 0 && s.cfg.WeKnora.RootTenantID != tenantID {
		if err := s.storage.SyncTenantModelsFromParent(ctx, s.cfg.WeKnora.RootTenantID, tenantID); err != nil {
			return fmt.Errorf("sync models: %w", err)
		}
	}
	if s.cfg.WeKnora.RootTenantID > 0 {
		if err := s.storage.SyncSpaceStorage(ctx, tenantID, s.cfg.WeKnora.RootTenantID); err != nil {
			return fmt.Errorf("sync storage: %w", err)
		}
		if err := s.storage.SyncSpaceSandbox(ctx, tenantID, s.cfg.WeKnora.RootTenantID); err != nil {
			return fmt.Errorf("sync sandbox: %w", err)
		}
	}
	return nil
}

// FindTargetDepartmentForUser 为用户解析其对应的同步空间部门 (优先当前部门是否是空间；否则沿 parentCode 向上追溯到空间部门)
func (s *Syncer) FindTargetDepartmentForUser(ctx context.Context, userDeptCode string) (*accountcenter.SysDepartment, error) {
	s.deptCacheLock.RLock()
	hasCache := len(s.deptMap) > 0
	s.deptCacheLock.RUnlock()

	if !hasCache {
		_ = s.SyncAll(ctx)
	}

	s.deptCacheLock.RLock()
	defer s.deptCacheLock.RUnlock()

	currCode := userDeptCode
	for currCode != "" {
		if target, ok := s.targetDepts[currCode]; ok {
			return &target, nil
		}
		parent, ok := s.deptMap[currCode]
		if !ok {
			break
		}
		currCode = strings.TrimSpace(parent.ParentCode)
	}
	return nil, nil
}

// SyncUserAndMemberships 同步员工及空间归属
func (s *Syncer) SyncUserAndMemberships(ctx context.Context, u *accountcenter.SysUser, targetDept *accountcenter.SysDepartment, isLeader bool) error {
	if u.UserStatus == 0 {
		return nil // 禁用用户跳过
	}

	phone := strings.TrimSpace(u.Mobile)
	email := strings.TrimSpace(u.Email)
	if email == "" && phone != "" {
		email = fmt.Sprintf("%s@benlai.com", phone)
	}
	if email == "" && u.UnionNo != "" {
		email = fmt.Sprintf("%s@wecom.local", strings.ToLower(u.UnionNo))
	}
	if email == "" {
		email = fmt.Sprintf("user_%s@wecom.local", u.ID)
	}

	username := strings.TrimSpace(u.UserName)
	if username == "" {
		username = u.UnionNo
	}
	if username == "" {
		username = u.EmployeeID
	}

	// 1. 同步或创建 WeKnoraUser
	existingUser, err := s.storage.GetUserByEmailOrUsername(ctx, email, username)
	if err != nil || existingUser == nil {
		if u.UnionNo != "" {
			existingUser, _ = s.storage.GetUserByEmailOrUsername(ctx, fmt.Sprintf("%s@wecom.local", strings.ToLower(u.UnionNo)), u.UnionNo)
		}
	}

	var userID string
	var homeTenantID uint64 = s.cfg.WeKnora.RootTenantID

	// 确定员工主空间：如果解析到了目标部门，获取其对应租户空间 ID
	var targetTenantID uint64
	if targetDept != nil {
		if mapping, err := s.storage.FindTenantByDeptCode(ctx, targetDept.DepartmentCode); err == nil && mapping != nil {
			targetTenantID = mapping.TenantID
			homeTenantID = mapping.TenantID
		}
	}

	// 构建员工偏好扩展字段 (保存手机号、企微工号、员工编号、部门编码)
	prefsMap := map[string]interface{}{}
	if phone != "" {
		prefsMap["phone"] = phone
	}
	if u.UnionNo != "" {
		prefsMap["wecom_userid"] = u.UnionNo
	}
	if u.EmployeeID != "" {
		prefsMap["employee_id"] = u.EmployeeID
	}
	if u.DepartmentCode != "" {
		prefsMap["department_code"] = u.DepartmentCode
	}
	prefsBytes, _ := json.Marshal(prefsMap)
	prefsJSON := string(prefsBytes)

	if existingUser != nil {
		userID = existingUser.ID
		existingUser.Username = username
		existingUser.Email = email
		existingUser.Preferences = prefsJSON
		if existingUser.TenantID == 0 && homeTenantID > 0 {
			existingUser.TenantID = homeTenantID
		}
		if err := s.storage.UpsertUser(ctx, existingUser); err != nil {
			log.Printf("[Syncer] 更新用户失败: %v", err)
		}
	} else {
		userID = uuid.New().String()
		randomPass, _ := bcrypt.GenerateFromPassword([]byte(uuid.New().String()), bcrypt.DefaultCost)

		newUser := &model.WeKnoraUser{
			ID:                  userID,
			Username:            username,
			Email:               email,
			PasswordHash:        string(randomPass),
			TenantID:            homeTenantID,
			IsActive:            true,
			CanAccessAllTenants: false,
			IsSystemAdmin:       false,
			Preferences:         prefsJSON,
			CreatedAt:           time.Now(),
			UpdatedAt:           time.Now(),
		}
		if err := s.storage.UpsertUser(ctx, newUser); err != nil {
			return fmt.Errorf("create user: %w", err)
		}
	}

	// 2. 绑定部门空间成员角色
	if targetTenantID > 0 {
		role := s.cfg.WeKnora.DefaultRole
		if role == "" {
			role = "contributor"
		}
		if isLeader {
			role = s.cfg.WeKnora.LeaderRole
			if role == "" {
				role = "admin"
			}
		}
		if err := s.storage.EnsureTenantMembership(ctx, userID, targetTenantID, role); err != nil {
			log.Printf("[Syncer] 绑定用户空间成员失败 [%s -> tenant %d]: %v", userID, targetTenantID, err)
		}
	}

	// 3. 保证员工属于公司公共全员空间 (如: 10001 本来生活)，固定为普通协作员
	if s.cfg.WeKnora.RootTenantID > 0 {
		if err := s.storage.EnsureTenantMembership(ctx, userID, s.cfg.WeKnora.RootTenantID, "contributor"); err != nil {
			log.Printf("[Syncer] 绑定用户公共全员空间失败: %v", err)
		}
	}

	return nil
}
