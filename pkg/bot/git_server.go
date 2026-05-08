package bot

// =====================================================================
// 주간 정리 — 분석 대상 레포 정적 상수
// =====================================================================
//
// 이 슬라이스의 각 항목이 [주간 정리] 클릭 시 1개 버튼으로 직접 노출된다.
// 추가/변경은 weeklyRepos만 손대면 된다 — 라우팅이나 핸들러 변경 불필요.
//
// 분석 단위는 모든 레포 동일: 현재 open 이슈 + 지난 N일(default 14일) 커밋 dump.
// release 단위 분기는 폐기됨 (release를 안 찍을 때도 많아서 일관 입력이 안정).

// WeeklyRepo는 [주간 정리] 버튼 한 칸의 정의.
type WeeklyRepo struct {
	Owner string
	Name  string
	Label string
}

// weeklyRepos는 [주간 정리]에서 노출할 레포 목록.
// 보틀노트 3개 (워크스페이스 / API / FE) + 개인 인프라(k8s-platform).
var weeklyRepos = []WeeklyRepo{
	{Owner: "bottle-note", Name: "workspace", Label: "워크스페이스"},
	{Owner: "bottle-note", Name: "bottle-note-api-server", Label: "API 서버"},
	{Owner: "bottle-note", Name: "bottle-note-frontend", Label: "프론트엔드"},
	{Owner: "Whale0928", Name: "k8s-platform", Label: "k8s-platform"},
}
