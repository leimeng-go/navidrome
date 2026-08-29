package model

import (
	"time"
)

// User 代表一个用户账号。Navidrome 是多用户系统，
// 每个用户拥有独立的播放次数、收藏、播放列表与可访问库范围。
type User struct {
	ID       string `structs:"id" json:"id"`
	UserName string `structs:"user_name" json:"userName"`
	Name     string `structs:"name" json:"name"` // 展示名
	Email    string `structs:"email" json:"email"`
	IsAdmin  bool   `structs:"is_admin" json:"isAdmin"`
	// LastLoginAt 上次登录时间；LastAccessAt 上次任意请求时间（用于活跃度统计）
	LastLoginAt  *time.Time `structs:"last_login_at" json:"lastLoginAt"`
	LastAccessAt *time.Time `structs:"last_access_at" json:"lastAccessAt"`
	CreatedAt    time.Time  `structs:"created_at" json:"createdAt"`
	UpdatedAt    time.Time  `structs:"updated_at" json:"updatedAt"`

	// Library associations (many-to-many relationship)
	// 可访问的库列表（多对多关联，存于关联表而非本表）
	Libraries Libraries `structs:"-" json:"libraries,omitempty"`

	// This is only available on the backend, and it is never sent over the wire
	// 仅后端可见，两个标签都是 "-"，确保既不入库也不出现在 API 响应中
	Password string `structs:"-" json:"-"`
	// This is used to set or change a password when calling Put. If it is empty, the password is not changed.
	// It is received from the UI with the name "password"
	// 调用 Put 时用于设置或修改密码；为空表示不改动密码。前端以 "password" 字段提交
	NewPassword string `structs:"password,omitempty" json:"password,omitempty"`
	// If changing the password, this is also required
	// 修改密码时必须一并提供当前密码以完成校验
	CurrentPassword string `structs:"current_password,omitempty" json:"currentPassword,omitempty"`
}

// HasLibraryAccess 判断用户是否可访问指定库。管理员默认拥有全部库权限。
func (u User) HasLibraryAccess(libraryID int) bool {
	if u.IsAdmin {
		return true // Admin users have access to all libraries
	}
	for _, lib := range u.Libraries {
		if lib.ID == libraryID {
			return true
		}
	}
	return false
}

type Users []User

// UserRepository 是用户仓储接口。
type UserRepository interface {
	ResourceRepository
	CountAll(...QueryOptions) (int64, error)
	Delete(id string) error
	Get(id string) (*User, error)
	// Put 新增或更新用户；若 NewPassword 非空则同时更新密码
	Put(*User) error
	UpdateLastLoginAt(id string) error
	UpdateLastAccessAt(id string) error
	// FindFirstAdmin 返回最早创建的管理员，播放列表自动导入等无用户上下文的
	// 后台流程会以该账号作为归属者
	FindFirstAdmin() (*User, error)
	// FindByUsername must be case-insensitive
	// FindByUsername 必须做大小写不敏感匹配
	FindByUsername(username string) (*User, error)
	// FindByUsernameWithPassword is the same as above, but also returns the decrypted password
	// FindByUsernameWithPassword 同上，但额外返回解密后的密码，仅用于登录校验
	FindByUsernameWithPassword(username string) (*User, error)

	// Library association methods
	// 用户与库的关联管理
	GetUserLibraries(userID string) (Libraries, error)
	// SetUserLibraries 全量覆盖用户的可访问库列表
	SetUserLibraries(userID string, libraryIDs []int) error
}
