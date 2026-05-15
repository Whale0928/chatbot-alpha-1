package bot

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/bwmarrin/discordgo"
)

// =====================================================================
// Discord guild role 조회 — super-session 모델의 ground truth role 분류 토대
//
// 설계:
//   - pure 함수 (mapRoleIDsToNames, ...) — discordgo 의존성 없이 unit test 가능
//   - I/O 함수 (fetchGuildRoleSnapshot, fetchUserRoles) — discordgo 직접 호출
//   - role snapshot은 세션 OPEN 시점 1회 박제 후 세션 lifetime 동안 고정
// =====================================================================

// mapRoleIDsToNames는 role ID slice를 사람이 읽는 role name slice로 변환한다 (pure).
// roleNames 맵에 없는 ID는 결과에서 제외 (예: 봇 자체 role 등 일반적으로 무관).
// 결과 순서는 입력 ids 순서를 보존.
func mapRoleIDsToNames(roleNames map[string]string, ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if name, ok := roleNames[id]; ok {
			out = append(out, name)
		}
	}
	return out
}

// buildRoleNameMap은 *discordgo.Guild의 Roles에서 ID→Name 매핑을 만든다 (pure 형태).
func buildRoleNameMap(roles []*discordgo.Role) map[string]string {
	out := make(map[string]string, len(roles))
	for _, r := range roles {
		if r == nil {
			continue
		}
		out[r.ID] = r.Name
	}
	return out
}

// fetchGuildRoleSnapshot은 guildID의 role 정의 + 지정 사용자들의 role 매핑을 만든다.
// 세션 OPEN 시 1회 호출 후 Session.RolesSnapshot에 박제.
//
// 반환 map: userID → []roleName  (예: "user_123" → ["BACKEND", "PM"])
//
// userIDs가 비어 있으면 호출 자체를 스킵하고 빈 map 반환 (Discord API 비용 절감).
// 개별 멤버 fetch가 실패해도 나머지는 진행 — log warn 후 해당 userID는 결과에서 누락.
func fetchGuildRoleSnapshot(s *discordgo.Session, guildID string, userIDs []string) (map[string][]string, error) {
	if len(userIDs) == 0 {
		return map[string][]string{}, nil
	}
	guild, err := s.Guild(guildID)
	if err != nil {
		return nil, fmt.Errorf("[roles] fetch guild %q: %w", guildID, err)
	}
	roleNames := buildRoleNameMap(guild.Roles)

	out := make(map[string][]string, len(userIDs))
	for _, uid := range userIDs {
		member, err := s.GuildMember(guildID, uid)
		if err != nil {
			log.Printf("[roles] WARN guild=%s user=%s 멤버 조회 실패: %v (skip)", guildID, uid, err)
			continue
		}
		out[uid] = mapRoleIDsToNames(roleNames, member.Roles)
	}
	return out, nil
}

// fetchUserRoles는 단일 사용자의 현재 role을 조회한다.
// 세션 OPEN 후 새로 등장한 발화자의 AuthorRoles 채울 때 사용 (snapshot은 세션 시작 시점 고정이라
// 새 발화자 role은 발화 시점 fetch).
//
// 실패 시 빈 slice 반환 + 에러 (호출자가 nil-check로 fallback 가능).
func fetchUserRoles(s *discordgo.Session, guildID, userID string) ([]string, error) {
	if guildID == "" || userID == "" {
		return nil, nil
	}
	guild, err := s.Guild(guildID)
	if err != nil {
		return nil, fmt.Errorf("[roles] fetch guild %q: %w", guildID, err)
	}
	member, err := s.GuildMember(guildID, userID)
	if err != nil {
		return nil, fmt.Errorf("[roles] fetch member %q in %q: %w", userID, guildID, err)
	}
	return mapRoleIDsToNames(buildRoleNameMap(guild.Roles), member.Roles), nil
}

// =====================================================================
// JSON 직렬화 — DB sessions.roles_snapshot 컬럼 저장/복원
// =====================================================================

// MarshalRoleSnapshot은 role snapshot map을 DB 저장용 JSON bytes로 변환한다.
// nil/빈 map은 "{}" 반환 (NOT NULL 컬럼 default와 일치).
func MarshalRoleSnapshot(m map[string][]string) ([]byte, error) {
	if len(m) == 0 {
		return []byte("{}"), nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("[roles] marshal snapshot: %w", err)
	}
	return b, nil
}

// UnmarshalRoleSnapshot은 DB sessions.roles_snapshot JSON을 map으로 복원한다.
// 빈 bytes / "{}" → 빈 map (nil 아님 — 호출자 lookup 안전).
func UnmarshalRoleSnapshot(raw []byte) (map[string][]string, error) {
	out := map[string][]string{}
	if len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("[roles] unmarshal snapshot: %w (raw=%q)", err, string(raw))
	}
	return out, nil
}

// MarshalAuthorRoles는 단일 발화자의 role slice를 DB notes.author_roles 저장용 JSON으로 변환한다.
// nil/빈 slice → "[]".
func MarshalAuthorRoles(roles []string) ([]byte, error) {
	if len(roles) == 0 {
		return []byte("[]"), nil
	}
	b, err := json.Marshal(roles)
	if err != nil {
		return nil, fmt.Errorf("[roles] marshal author_roles: %w", err)
	}
	return b, nil
}
