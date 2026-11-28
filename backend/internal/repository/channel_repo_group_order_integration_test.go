//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/suite"
)

// ChannelGroupOrderSuite 验证后台渠道列表按分组顺序（sort_by=group_order）排序的真实查询结果。
// 渠道写入自管事务，无法用 ent tx 隔离，因此每个用例前清空 channels/channel_groups，
// 并记录本用例创建的分组用于收尾删除，保证 total 等于本用例渠道数。
type ChannelGroupOrderSuite struct {
	suite.Suite
	ctx      context.Context
	repo     *channelRepository
	groupIDs []int64
}

func TestChannelGroupOrderSuite(t *testing.T) {
	suite.Run(t, new(ChannelGroupOrderSuite))
}

func (s *ChannelGroupOrderSuite) SetupTest() {
	s.ctx = context.Background()
	s.repo = &channelRepository{db: integrationDB}
	s.groupIDs = nil
	// 清空上一个用例可能遗留的数据，channel_groups 随 channels ON DELETE CASCADE 清除。
	_, err := integrationDB.ExecContext(s.ctx, "DELETE FROM channels")
	s.Require().NoError(err)
}

func (s *ChannelGroupOrderSuite) TearDownTest() {
	_, err := integrationDB.ExecContext(s.ctx, "DELETE FROM channels")
	s.Require().NoError(err)
	for _, id := range s.groupIDs {
		_, err := integrationDB.ExecContext(s.ctx, "DELETE FROM groups WHERE id = $1", id)
		s.Require().NoError(err)
	}
}

// insertGroup 直接写入分组以精确控制 sort_order、status、deleted_at 与插入顺序（决定 id）。
func (s *ChannelGroupOrderSuite) insertGroup(name string, sortOrder int, status string, deleted bool) int64 {
	var deletedAt any
	if deleted {
		deletedAt = time.Now().UTC()
	}
	var id int64
	err := integrationDB.QueryRowContext(s.ctx,
		`INSERT INTO groups (name, sort_order, status, deleted_at) VALUES ($1, $2, $3, $4) RETURNING id`,
		name, sortOrder, status, deletedAt,
	).Scan(&id)
	s.Require().NoError(err)
	s.groupIDs = append(s.groupIDs, id)
	return id
}

// createChannel 创建渠道并绑定分组，返回其自增 id（创建顺序决定 id 大小）。
func (s *ChannelGroupOrderSuite) createChannel(name, status string, groupIDs ...int64) int64 {
	ch := &service.Channel{
		Name:               name,
		Status:             status,
		BillingModelSource: service.BillingModelSourceRequested,
		GroupIDs:           groupIDs,
	}
	s.Require().NoError(s.repo.Create(s.ctx, ch))
	return ch.ID
}

// listGroupOrder 取 group_order 排序下的全部渠道 id（按返回顺序）与 total。
func (s *ChannelGroupOrderSuite) listGroupOrder(status, search, sortOrder string) ([]int64, int64) {
	chans, res, err := s.repo.List(s.ctx,
		pagination.PaginationParams{Page: 1, PageSize: 100, SortBy: "group_order", SortOrder: sortOrder},
		status, search,
	)
	s.Require().NoError(err)
	ids := make([]int64, 0, len(chans))
	for _, c := range chans {
		ids = append(ids, c.ID)
	}
	return ids, res.Total
}

func (s *ChannelGroupOrderSuite) TestSingleAndMultiGroup() {
	// g1 sort 30, g2 sort 10, g3 sort 20, g4 sort 10（与 g2 同值，id 更大）
	g1 := s.insertGroup("cgo-s-g1", 30, service.StatusActive, false)
	g2 := s.insertGroup("cgo-s-g2", 10, service.StatusActive, false)
	g3 := s.insertGroup("cgo-s-g3", 20, service.StatusActive, false)
	g4 := s.insertGroup("cgo-s-g4", 10, service.StatusActive, false)

	cA := s.createChannel("cgo-s-a", service.StatusActive, g1)     // 代表 sort=30
	cB := s.createChannel("cgo-s-b", service.StatusActive, g3)     // 代表 sort=20
	cC := s.createChannel("cgo-s-c", service.StatusActive, g2, g4) // 多分组：sort 同为 10，取 id 更小的 g2

	ids, total := s.listGroupOrder("", "", "asc")
	s.Require().Equal(int64(3), total)
	// 升序：cC(10,g2) < cB(20,g3) < cA(30,g1)
	s.Require().Equal([]int64{cC, cB, cA}, ids)
}

func (s *ChannelGroupOrderSuite) TestRepresentativeComesFromSameRecord() {
	// 构造「sort_order 最小的分组」与「id 最小的分组」不是同一条：
	// gLow 先插入（id 较小）但 sort_order=10；gHigh 后插入（id 较大）但 sort_order=5。
	gLow := s.insertGroup("cgo-r-low", 10, service.StatusActive, false)
	gHigh := s.insertGroup("cgo-r-high", 5, service.StatusActive, false)
	gMid := s.insertGroup("cgo-r-mid", 7, service.StatusActive, false)

	cX := s.createChannel("cgo-r-x", service.StatusActive, gLow, gHigh) // 代表应为 gHigh(sort=5)
	cY := s.createChannel("cgo-r-y", service.StatusActive, gMid)        // sort=7

	ids, _ := s.listGroupOrder("", "", "asc")
	// 若错误地按 id 最小的 gLow(sort=10) 作代表，cX 会排到 cY 之后。cX 在前证明代表来自 sort_order 最小的那条记录。
	s.Require().Equal([]int64{cX, cY}, ids)
}

func (s *ChannelGroupOrderSuite) TestTieSortOrderBreaksByGroupID() {
	// 重复 sort_order：代表相同 sort_order 时按分组 id 升序。
	gEarly := s.insertGroup("cgo-t-early", 15, service.StatusActive, false) // id 较小
	gLate := s.insertGroup("cgo-t-late", 15, service.StatusActive, false)   // id 较大

	cLate := s.createChannel("cgo-t-late-ch", service.StatusActive, gLate)
	cEarly := s.createChannel("cgo-t-early-ch", service.StatusActive, gEarly)

	ids, _ := s.listGroupOrder("", "", "asc")
	// 同 sort_order=15，按分组 id 升序：gEarly 的渠道在前，与渠道创建顺序无关。
	s.Require().Equal([]int64{cEarly, cLate}, ids)
}

func (s *ChannelGroupOrderSuite) TestNegativeSortOrder() {
	gNeg := s.insertGroup("cgo-n-neg", -5, service.StatusActive, false)
	gZero := s.insertGroup("cgo-n-zero", 0, service.StatusActive, false)
	gPos := s.insertGroup("cgo-n-pos", 5, service.StatusActive, false)

	cPos := s.createChannel("cgo-n-pos-ch", service.StatusActive, gPos)
	cNeg := s.createChannel("cgo-n-neg-ch", service.StatusActive, gNeg)
	cZero := s.createChannel("cgo-n-zero-ch", service.StatusActive, gZero)

	ids, _ := s.listGroupOrder("", "", "asc")
	s.Require().Equal([]int64{cNeg, cZero, cPos}, ids)
}

func (s *ChannelGroupOrderSuite) TestNoEligibleGroupsSortLastByChannelID() {
	gActive := s.insertGroup("cgo-e-active", 10, service.StatusActive, false)
	gDisabled := s.insertGroup("cgo-e-disabled", 1, service.StatusDisabled, false)
	gDeleted := s.insertGroup("cgo-e-deleted", 1, service.StatusActive, true)

	cWithGroup := s.createChannel("cgo-e-with", service.StatusActive, gActive)
	// 以下三个渠道都没有合格分组，应按渠道 id 升序排在有合格分组者之后。
	cNone := s.createChannel("cgo-e-none", service.StatusActive)
	cOnlyDisabled := s.createChannel("cgo-e-dis", service.StatusActive, gDisabled)
	cOnlyDeleted := s.createChannel("cgo-e-del", service.StatusActive, gDeleted)

	ids, total := s.listGroupOrder("", "", "asc")
	s.Require().Equal(int64(4), total)
	s.Require().Equal([]int64{cWithGroup, cNone, cOnlyDisabled, cOnlyDeleted}, ids)
}

func (s *ChannelGroupOrderSuite) TestDisabledChannelsAfterActive() {
	gActiveHi := s.insertGroup("cgo-d-hi", 50, service.StatusActive, false)
	gActiveLo := s.insertGroup("cgo-d-lo", 1, service.StatusActive, false)

	// 停用渠道即使分组 sort_order 很小，也整体排在启用渠道之后。
	cDisabled := s.createChannel("cgo-d-disabled", service.StatusDisabled, gActiveLo)
	cActive := s.createChannel("cgo-d-active", service.StatusActive, gActiveHi)

	ids, _ := s.listGroupOrder("", "", "asc")
	s.Require().Equal([]int64{cActive, cDisabled}, ids)
}

func (s *ChannelGroupOrderSuite) TestPaginationConcatenationMatchesFull() {
	// 构造 7 个渠道，部分多分组、部分无合格分组。
	g1 := s.insertGroup("cgo-p-g1", 40, service.StatusActive, false)
	g2 := s.insertGroup("cgo-p-g2", 20, service.StatusActive, false)
	g3 := s.insertGroup("cgo-p-g3", 10, service.StatusActive, false)
	g4 := s.insertGroup("cgo-p-g4", 30, service.StatusActive, false)
	gDead := s.insertGroup("cgo-p-dead", 1, service.StatusDisabled, false)

	s.createChannel("cgo-p-a", service.StatusActive, g1)
	s.createChannel("cgo-p-b", service.StatusActive, g2, g3) // 代表 sort=10
	s.createChannel("cgo-p-c", service.StatusActive, g4)
	s.createChannel("cgo-p-d", service.StatusActive)        // 无分组
	s.createChannel("cgo-p-e", service.StatusActive, gDead) // 只绑停用分组
	s.createChannel("cgo-p-f", service.StatusDisabled)

	// 一次取全量
	full, total := s.listGroupOrder("", "", "asc")
	s.Require().Equal(int64(6), total)

	// 分页拼接（页大小 2）
	var paged []int64
	for page := 1; ; page++ {
		chans, res, err := s.repo.List(s.ctx,
			pagination.PaginationParams{Page: page, PageSize: 2, SortBy: "group_order", SortOrder: "asc"},
			"", "",
		)
		s.Require().NoError(err)
		s.Require().Equal(int64(6), res.Total, "total 不因多分组变大")
		if len(chans) == 0 {
			break
		}
		for _, c := range chans {
			paged = append(paged, c.ID)
		}
		if page > 10 {
			s.FailNow("pagination did not terminate")
		}
	}
	s.Require().Equal(full, paged, "分页拼接顺序必须与一次取全量一致，无重复或遗漏")
}

func (s *ChannelGroupOrderSuite) TestSearchAndStatusFilterWithGroupOrder() {
	gHi := s.insertGroup("cgo-f-hi", 40, service.StatusActive, false)
	gLo := s.insertGroup("cgo-f-lo", 10, service.StatusActive, false)
	gMid := s.insertGroup("cgo-f-mid", 20, service.StatusActive, false)

	cKeepHi := s.createChannel("keepme-hi", service.StatusActive, gHi)
	cKeepLo := s.createChannel("keepme-lo", service.StatusActive, gLo)
	s.createChannel("other", service.StatusActive, gMid)
	cKeepDisabled := s.createChannel("keepme-disabled", service.StatusDisabled)

	// 搜索 keepme + group_order：仅命中 keepme-*，按分组顺序升序。
	searchIDs, _ := s.listGroupOrder("", "keepme", "asc")
	s.Require().Equal([]int64{cKeepLo, cKeepHi, cKeepDisabled}, searchIDs)

	// 状态筛选 active：停用渠道被排除。
	activeIDs, total := s.listGroupOrder(service.StatusActive, "keepme", "asc")
	s.Require().Equal(int64(2), total)
	s.Require().Equal([]int64{cKeepLo, cKeepHi}, activeIDs)
}

func (s *ChannelGroupOrderSuite) TestReactsToSortOrderChange() {
	gA := s.insertGroup("cgo-c-a", 10, service.StatusActive, false)
	gB := s.insertGroup("cgo-c-b", 20, service.StatusActive, false)
	cA := s.createChannel("cgo-c-cha", service.StatusActive, gA)
	cB := s.createChannel("cgo-c-chb", service.StatusActive, gB)

	ids, _ := s.listGroupOrder("", "", "asc")
	s.Require().Equal([]int64{cA, cB}, ids)

	// 调整 gA 的 sort_order 使其排到 gB 之后，渠道顺序随之变化。
	_, err := integrationDB.ExecContext(s.ctx, "UPDATE groups SET sort_order = 30 WHERE id = $1", gA)
	s.Require().NoError(err)

	ids2, _ := s.listGroupOrder("", "", "asc")
	s.Require().Equal([]int64{cB, cA}, ids2)
}

func (s *ChannelGroupOrderSuite) TestGroupOrderIgnoresSortOrderDirection() {
	gA := s.insertGroup("cgo-dir-a", 10, service.StatusActive, false)
	gB := s.insertGroup("cgo-dir-b", 20, service.StatusActive, false)
	cA := s.createChannel("cgo-dir-cha", service.StatusActive, gA)
	cB := s.createChannel("cgo-dir-chb", service.StatusActive, gB)

	// asc、desc、省略都应得到同一顺序（固定升序约定）。
	for _, order := range []string{"asc", "desc", ""} {
		ids, _ := s.listGroupOrder("", "", order)
		s.Require().Equalf([]int64{cA, cB}, ids, "sort_order=%q", order)
	}
}

func (s *ChannelGroupOrderSuite) TestExplicitNameAndCreatedAtUnchanged() {
	g := s.insertGroup("cgo-x-g", 10, service.StatusActive, false)
	cZulu := s.createChannel("zulu", service.StatusActive)
	cAlpha := s.createChannel("alpha", service.StatusActive, g)

	// name 升序与改动前语义一致：按名称字母序，不走分组顺序，也不带 JOIN。
	chans, _, err := s.repo.List(s.ctx,
		pagination.PaginationParams{Page: 1, PageSize: 100, SortBy: "name", SortOrder: "asc"},
		"", "",
	)
	s.Require().NoError(err)
	s.Require().Len(chans, 2)
	s.Require().Equal("alpha", chans[0].Name)
	s.Require().Equal("zulu", chans[1].Name)

	// created_at 升序：alpha 后创建，排在后面。
	chans2, _, err := s.repo.List(s.ctx,
		pagination.PaginationParams{Page: 1, PageSize: 100, SortBy: "created_at", SortOrder: "asc"},
		"", "",
	)
	s.Require().NoError(err)
	s.Require().Len(chans2, 2)
	s.Require().Equal(cZulu, chans2[0].ID)
	s.Require().Equal(cAlpha, chans2[1].ID)
}

func (s *ChannelGroupOrderSuite) TestListAllStillByChannelID() {
	gB := s.insertGroup("cgo-la-b", 1, service.StatusActive, false)
	gA := s.insertGroup("cgo-la-a", 99, service.StatusActive, false)
	// 先创建代表 sort_order 大的，再创建代表 sort_order 小的，验证 ListAll 不受分组顺序影响。
	cFirst := s.createChannel("cgo-la-first", service.StatusActive, gA)
	cSecond := s.createChannel("cgo-la-second", service.StatusActive, gB)

	all, err := s.repo.ListAll(s.ctx)
	s.Require().NoError(err)
	s.Require().Len(all, 2)
	s.Require().Equal(cFirst, all[0].ID)
	s.Require().Equal(cSecond, all[1].ID)
}
