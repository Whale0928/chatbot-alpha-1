package bot

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"chatbot-alpha-1/pkg/github"
	"chatbot-alpha-1/pkg/llm"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
)

// =====================================================================
// 세션 타입 / 상태
// =====================================================================

type SessionMode int

const (
	ModeNormal  SessionMode = iota
	ModeMeeting             // 미팅 모드
)

type SessionState int

const (
	StateSelectMode            SessionState = iota // 기능 선택 대기
	StateMeeting                                   // 미팅 진행 중 (조용히 수집)
	StateMeetingAwaitDirective                     // 미팅 종료 후 사용자 정리 지시 입력 대기
	StateWeeklyAwaitDirective                      // 주간 분석 결과 후 사용자 추가 요청 입력 대기
	StateAgentAwaitInput                           // [에이전트] 클릭 후 자유 지시 입력 대기
)

// Session은 디스코드 스레드와 1:1 매핑되는 대화 단위.
// 필드별 동시성 책임:
//   - NotesMu 가 보호: Notes, Speakers, InterimInFlight,
//     StickyMessageID, NotesAtLastSticky
//   - 그 외(Mode, State, NoteFormat, Directive, UpdatedAt 등)는 messageCreate/
//     interactionCreate 단일 이벤트 루프에서만 읽고 쓴다.
type Session struct {
	Mode      SessionMode
	State     SessionState
	ThreadID  string
	UserID    string
	NotesMu   sync.Mutex
	Notes     []Note
	Speakers  map[string]bool
	UpdatedAt time.Time

	// NoteFormat은 미팅 종료 시 생성할 노트 포맷.
	// 미팅 시작 시 default(FormatDecisionStatus). 사용자가 종료 시 4 버튼 중
	// 하나를 선택하기 직전까지의 잠정값. 실제 finalize는 인자로 명시 전달.
	NoteFormat llm.NoteFormat

	// Directive는 사용자가 [추가 요청] 흐름에서 입력한 정리 지시문.
	// 비어 있으면 기본 시스템 프롬프트만으로 정리한다. finalize 시점에
	// LLM user 메시지 앞에 prepend되어 시스템 프롬프트보다 후순위지만
	// 노트 본문보다 상위 컨텍스트로 작용한다.
	Directive string

	// === Interim 보고 트래킹 (NotesMu 보호) ===
	// InterimInFlight는 현재 emitInterim 호출이 진행 중인지 표시.
	// 사용자가 [중간 요약] 버튼을 빠르게 두 번 누를 때 두 번째 클릭을 거부하는 가드.
	InterimInFlight bool

	// === Sticky 컨트롤 메시지 트래킹 (NotesMu 보호) ===
	// 스티키 버튼 메시지의 Discord message ID. 갱신 시 delete 대상.
	StickyMessageID string
	// 마지막 sticky 발사 시점의 노트 개수.
	// (현재 노트 수 - NotesAtLastSticky >= threshold)일 때 재전송.
	NotesAtLastSticky int

	// === 주간 정리 follow-up 컨텍스트 ===
	// 마지막으로 수행한 주간 분석을 기억해 [다시 분석] / [추가 요청] / [기간 변경]
	// 같은 follow-up 버튼이 같은 레포·기간으로 재호출하거나 directive를 덮어쓸 수 있게 한다.
	// nil/zero 값은 "직전 주간 분석이 없음"을 의미.
	LastWeeklyRepo      string                    // "owner/name"
	LastWeeklySince     time.Time                 // 분석 시점 since
	LastWeeklyUntil     time.Time                 // 분석 시점 until
	LastWeeklyDirective string                    // 직전 directive (재분석 시 그대로 사용)
	LastWeeklyScope     llm.WeeklyScope           // 직전 분석 범위 (이슈/커밋/전체) — 재분석 시 유지
	LastWeeklyResponse  *llm.WeeklyReportResponse // [미팅 시작]에서 첫 노트로 주입
	LastWeeklyCloseable []llm.ClosableIssue       // [닫기] 액션의 ground truth — confirm/실행 시점에 LLM 재호출 없이 사용

	// PendingWeeklyRepo는 사용자가 [주간 정리]에서 레포를 선택한 직후, scope 버튼을 누르기
	// 전까지 임시 박제되는 fullName. scope 버튼 클릭 시 이 값을 꺼내 runWeeklyAnalyze에 전달한다.
	// scope 선택 prompt가 노출되는 짧은 윈도우에서만 의미 있고, 분석이 실행되면 LastWeekly* 로 옮겨진다.
	PendingWeeklyRepo string

	// LastBotSummary는 같은 스레드에서 직전에 봇이 전송한 마크다운 결과.
	// weekly 분석 markdown / 미팅 finalize markdown이 채운다.
	// 에이전트 모드가 사용자 지시에 "이전 대화"를 인용할 때 LLM에 함께 보낸다.
	// Pod 재시작 시 휘발(현재 세션 스토어 자체가 in-memory).
	LastBotSummary string
}

// =====================================================================
// 패키지 전역 상태
// =====================================================================
//
// 현재는 단일 bot 프로세스만 가정한다. 여러 인스턴스 동시 구동이 필요해지면
// 이 전역들을 Bot 구조체 필드로 옮겨 인스턴스화해야 한다.
var (
	sessions     = make(map[string]*Session)
	sessionsMu   sync.RWMutex
	botChannelID string
	llmClient    *llm.Client       // raw SDK 클라이언트 (현재 직접 호출 안 함, summarizer 어댑터 통해 사용)
	summarizer   MeetingSummarizer // llmSummarizer{c: llmClient} — 미팅 요약/interim 호출 진입점

	// githubClient는 주간 정리에서 org repo / 이슈를 조회할 때 사용한다.
	// GITHUB_TOKEN 미설정 시 nil 그대로 두고, 주간 정리 핸들러가 nil 가드로 안내 메시지를 보낸다.
	githubClient *github.Client
	// githubOrg는 ListOrgRepos 대상 organization slug. .env의 GITHUB_ORG에서 읽고
	// 비었으면 default "bottle-note".
	githubOrg string
)

// =====================================================================
// Run: 봇 엔트리포인트
// =====================================================================

// Run은 Discord 봇을 기동하고 SIGINT/SIGTERM까지 블록한다.
// envFile이 비어있지 않으면 해당 경로의 .env를 로드한다 (파일이 없어도 에러 아님).
// 반환하는 에러는 cobra RunE에서 처리한다.
func Run(envFile string) error {
	loadEnvWithLog(envFile)

	token := os.Getenv("DISCORD_BOT_TOKEN")
	if token == "" {
		return fmt.Errorf("DISCORD_BOT_TOKEN 환경변수가 필요합니다")
	}
	botChannelID = os.Getenv("BOT_CHANNEL_ID")

	// GPT_API_KEY만 사용. OPENAI_API_KEY에 의존하지 않음.
	gptKey := os.Getenv("GPT_API_KEY")
	if gptKey == "" {
		return fmt.Errorf("GPT_API_KEY 환경변수가 필요합니다")
	}
	c, err := llm.NewClient(gptKey)
	if err != nil {
		return fmt.Errorf("LLM 클라이언트 초기화 실패: %w", err)
	}
	llmClient = c
	summarizer = llmSummarizer{c: c}
	log.Println("LLM 클라이언트 초기화 완료 (gpt-5.5 + reasoning=medium)")

	// GitHub 클라이언트는 GITHUB_TOKEN이 있을 때만 활성화. 없으면 주간 정리는 비활성화 안내만.
	githubOrg = os.Getenv("GITHUB_ORG")
	if githubOrg == "" {
		githubOrg = "bottle-note"
	}
	if ghToken := os.Getenv("GITHUB_TOKEN"); ghToken != "" {
		gc, gerr := github.NewClient(ghToken)
		if gerr != nil {
			return fmt.Errorf("GitHub 클라이언트 초기화 실패: %w", gerr)
		}
		githubClient = gc
		log.Printf("GitHub 클라이언트 초기화 완료 (org=%s, 주간 정리 레포 %d개)", githubOrg, len(weeklyRepos))
	} else {
		log.Println("GITHUB_TOKEN 미설정 — 주간 정리 기능 비활성화")
	}

	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return fmt.Errorf("Discord 세션 생성 실패: %w", err)
	}

	s.AddHandler(messageCreate)
	s.AddHandler(interactionCreate)
	s.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsMessageContent

	if err := s.Open(); err != nil {
		return fmt.Errorf("Discord 연결 실패: %w", err)
	}
	defer s.Close()

	// 슬래시 명령어 등록은 Open 직후 (s.State.User.ID 채워진 뒤). guildID 비면 글로벌(전파 1h),
	// 비어있지 않으면 해당 길드에 즉시 반영. 동일 이름은 덮어쓰므로 cleanup 불필요.
	guildID := os.Getenv("DISCORD_GUILD_ID")
	if err := registerSlashCommands(s, guildID); err != nil {
		return fmt.Errorf("슬래시 명령어 등록 실패: %w", err)
	}

	go cleanupSessions()

	fmt.Printf("봇 온라인: %s#%s\n", s.State.User.Username, s.State.User.Discriminator)
	if botChannelID != "" {
		fmt.Printf("전용 채널: %s\n", botChannelID)
	}
	fmt.Println("종료하려면 Ctrl+C")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	fmt.Println("봇 종료")
	return nil
}

// =====================================================================
// 세션 관리 / 범용 유틸
// =====================================================================

// lookupSession은 channelID에 대응하는 세션을 안전하게 조회한다.
// 세션이 없으면 nil 반환. 호출자는 이 결과만으로 "봇이 책임지는 대화"인지 판단할 수 있다.
func lookupSession(channelID string) *Session {
	sessionsMu.RLock()
	defer sessionsMu.RUnlock()
	if sess, ok := sessions[channelID]; ok {
		return sess
	}
	return nil
}

func cleanupSessions() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		sessionsMu.Lock()
		now := time.Now()
		for id, sess := range sessions {
			timeout := 5 * time.Minute
			if sess.Mode == ModeMeeting {
				timeout = 2 * time.Hour
			}
			if now.Sub(sess.UpdatedAt) > timeout {
				mode := "normal"
				if sess.Mode == ModeMeeting {
					mode = "meeting"
				}
				log.Printf("[세션/만료] thread=%s mode=%s notes=%d idle=%s", id, mode, len(sess.Notes), now.Sub(sess.UpdatedAt).Round(time.Second))
				delete(sessions, id)
			}
		}
		sessionsMu.Unlock()
	}
}

// loadEnvWithLog는 .env를 로드하고 결과를 명시적으로 로그로 남긴다.
// 기존엔 _ = godotenv.Load()로 silent fail이라 "왜 환경변수가 안 잡혔지?" 진단이 어려웠다.
//
// envFile이 명시되면 그 경로만 시도하고, 비었으면 default(현재 cwd의 .env).
// 실패해도 에러를 반환하지 않는다 — 환경 변수가 이미 export되어 있을 수도 있기 때문.
func loadEnvWithLog(envFile string) {
	cwd, _ := os.Getwd()
	target := envFile
	if target == "" {
		target = ".env"
	}
	if err := godotenv.Load(target); err != nil {
		log.Printf("[env] WARN %s 로드 실패 (cwd=%s): %v — OS 환경변수만 사용", target, cwd, err)
		return
	}
	log.Printf("[env] %s 로드 완료 (cwd=%s)", target, cwd)
}

func truncate(s string, maxLen int) string {
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	return string(r[:maxLen])
}

func stripMention(content, botID string) string {
	content = strings.ReplaceAll(content, "<@"+botID+">", "")
	content = strings.ReplaceAll(content, "<@!"+botID+">", "")
	return strings.TrimSpace(content)
}
