package persistence

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	. "github.com/Masterminds/squirrel"
	"github.com/deluan/rest"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/id"
	"github.com/navidrome/navidrome/utils"
	"github.com/navidrome/navidrome/utils/slice"
	"github.com/pocketbase/dbx"
)

// userRepository 是用户仓储，兼管密码加密与音乐库授权关系。
type userRepository struct {
	sqlRepository
}

// dbUser 是 model.User 的数据库映射层，
// 附带把用户可访问的音乐库聚合成的 JSON。
type dbUser struct {
	*model.User   `structs:",flatten"`
	LibrariesJSON string `structs:"-" json:"-"`
}

// PostScan 解析用户可访问的音乐库列表。
func (u *dbUser) PostScan() error {
	if u.LibrariesJSON != "" {
		if err := json.Unmarshal([]byte(u.LibrariesJSON), &u.User.Libraries); err != nil {
			return fmt.Errorf("parsing user libraries from db: %w", err)
		}
	}
	return nil
}

type dbUsers []dbUser

func (us dbUsers) toModels() model.Users {
	return slice.Map(us, func(u dbUser) model.User { return *u.User })
}

// 密码加密密钥全进程只初始化一次。
var (
	once   sync.Once
	encKey []byte
)

// NewUserRepository 创建用户仓储。
// password 注册为非法过滤字段，杜绝通过 REST 查询参数按密码检索。
func NewUserRepository(ctx context.Context, db dbx.Builder) model.UserRepository {
	r := &userRepository{}
	r.ctx = ctx
	r.db = db
	r.tableName = "user"
	r.registerModel(&model.User{}, map[string]filterFunc{
		"id":       idFilter(r.tableName),
		"password": invalidFilter(ctx),
		"name":     r.withTableName(startsWithFilter),
	})
	once.Do(func() {
		_ = r.initPasswordEncryptionKey()
	})
	return r
}

// selectUserWithLibraries returns a SelectBuilder that includes library information
// selectUserWithLibraries 构建查询并把用户可访问的音乐库聚合为 JSON 数组。
// FILTER (WHERE library.id IS NOT NULL) 用于排除 LEFT JOIN 产生的空行，
// 否则未授权任何库的用户会得到 [null] 而非 []。
func (r *userRepository) selectUserWithLibraries(options ...model.QueryOptions) SelectBuilder {
	return r.newSelect(options...).
		Columns(`user.*`,
			`COALESCE(json_group_array(json_object(
				'id', library.id,
				'name', library.name,
				'path', library.path,
				'remote_path', library.remote_path,
				'last_scan_at', library.last_scan_at,
				'last_scan_started_at', library.last_scan_started_at,
				'full_scan_in_progress', library.full_scan_in_progress,
				'updated_at', library.updated_at,
				'created_at', library.created_at
			)) FILTER (WHERE library.id IS NOT NULL), '[]') AS libraries_json`).
		LeftJoin("user_library ul ON user.id = ul.user_id").
		LeftJoin("library ON ul.library_id = library.id").
		GroupBy("user.id")
}

// CountAll 统计用户总数。
func (r *userRepository) CountAll(qo ...model.QueryOptions) (int64, error) {
	return r.count(Select(), qo...)
}

// Get 按 ID 读取用户（含可访问的音乐库）。
func (r *userRepository) Get(id string) (*model.User, error) {
	sel := r.selectUserWithLibraries().Where(Eq{"user.id": id})
	var res dbUser
	err := r.queryOne(sel, &res)
	if err != nil {
		return nil, err
	}
	return res.User, nil
}

// GetAll 查询全部用户。
func (r *userRepository) GetAll(options ...model.QueryOptions) (model.Users, error) {
	sel := r.selectUserWithLibraries(options...)
	var res dbUsers
	err := r.queryAll(sel, &res)
	if err != nil {
		return nil, err
	}
	return res.toModels(), nil
}

// Put 保存用户，并维护其音乐库授权。
//
// 这里不用通用的 put：需要先尝试 UPDATE、依据影响行数判断是否为新用户，
// 因为后续的库授权逻辑对新老用户处理不同。
// current_password 只用于校验，不能落库。
//
// 授权规则：管理员每次保存都补齐全部库（新增库后管理员应自动可见）；
// 普通用户只在创建时授予标记为「新用户默认」的库，
// 之后的授权变更交由管理界面显式管理，避免覆盖手工调整。
func (r *userRepository) Put(u *model.User) error {
	if u.ID == "" {
		u.ID = id.NewRandom()
	}
	u.UpdatedAt = time.Now()
	if u.NewPassword != "" {
		_ = r.encryptPassword(u)
	}
	values, err := toSQLArgs(*u)
	if err != nil {
		return fmt.Errorf("error converting user to SQL args: %w", err)
	}
	delete(values, "current_password")

	// Save/update the user
	update := Update(r.tableName).Where(Eq{"id": u.ID}).SetMap(values)
	count, err := r.executeSQL(update)
	if err != nil {
		return err
	}

	isNewUser := count == 0
	if isNewUser {
		values["created_at"] = time.Now()
		insert := Insert(r.tableName).SetMap(values)
		_, err = r.executeSQL(insert)
		if err != nil {
			return err
		}
	}

	// Auto-assign all libraries to admin users in a single SQL operation
	if u.IsAdmin {
		sql := Expr(
			"INSERT OR IGNORE INTO user_library (user_id, library_id) SELECT ?, id FROM library",
			u.ID,
		)
		if _, err := r.executeSQL(sql); err != nil {
			return fmt.Errorf("failed to assign all libraries to admin user: %w", err)
		}
	} else if isNewUser { // Only for new regular users
		// Auto-assign default libraries to new regular users
		sql := Expr(
			"INSERT OR IGNORE INTO user_library (user_id, library_id) SELECT ?, id FROM library WHERE default_new_users = true",
			u.ID,
		)
		if _, err := r.executeSQL(sql); err != nil {
			return fmt.Errorf("failed to assign default libraries to new user: %w", err)
		}
	}

	return nil
}

// FindFirstAdmin 返回最早创建的管理员（按 updated_at 取第一条），
// 用于需要一个「系统管理员」身份执行后台操作的场景。
func (r *userRepository) FindFirstAdmin() (*model.User, error) {
	sel := r.selectUserWithLibraries(model.QueryOptions{Sort: "updated_at", Max: 1}).Where(Eq{"user.is_admin": true})
	var usr dbUser
	err := r.queryOne(sel, &usr)
	if err != nil {
		return nil, err
	}
	return usr.User, nil
}

// FindByUsername 按用户名查找，大小写不敏感（COLLATE NOCASE），
// 以便用户登录时不必严格匹配大小写。
func (r *userRepository) FindByUsername(username string) (*model.User, error) {
	sel := r.selectUserWithLibraries().Where(Expr("user.user_name = ? COLLATE NOCASE", username))
	var usr dbUser
	err := r.queryOne(sel, &usr)
	if err != nil {
		return nil, err
	}
	return usr.User, nil
}

// FindByUsernameWithPassword 在 FindByUsername 基础上解密密码。
// Subsonic API 的部分认证方式需要明文密码参与校验，故必须可逆存储。
func (r *userRepository) FindByUsernameWithPassword(username string) (*model.User, error) {
	usr, err := r.FindByUsername(username)
	if err != nil {
		return nil, err
	}
	_ = r.decryptPassword(usr)
	return usr, nil
}

// UpdateLastLoginAt 记录最近一次登录时间。
func (r *userRepository) UpdateLastLoginAt(id string) error {
	upd := Update(r.tableName).Where(Eq{"id": id}).Set("last_login_at", time.Now())
	_, err := r.executeSQL(upd)
	return err
}

// UpdateLastAccessAt 记录最近一次访问时间，用于展示活跃状态。
func (r *userRepository) UpdateLastAccessAt(id string) error {
	now := time.Now()
	upd := Update(r.tableName).Where(Eq{"id": id}).Set("last_access_at", now)
	_, err := r.executeSQL(upd)
	return err
}

// 以下实现 rest 接口：用户数据敏感，每个方法都需先做权限判断。
// 除读取本人资料外，均限管理员操作。

func (r *userRepository) Count(options ...rest.QueryOptions) (int64, error) {
	usr := loggedUser(r.ctx)
	if !usr.IsAdmin {
		return 0, rest.ErrPermissionDenied
	}
	return r.CountAll(r.parseRestOptions(r.ctx, options...))
}

func (r *userRepository) Read(id string) (any, error) {
	usr := loggedUser(r.ctx)
	if !usr.IsAdmin && usr.ID != id {
		return nil, rest.ErrPermissionDenied
	}
	usr, err := r.Get(id)
	if errors.Is(err, model.ErrNotFound) {
		return nil, rest.ErrNotFound
	}
	return usr, err
}

func (r *userRepository) ReadAll(options ...rest.QueryOptions) (any, error) {
	usr := loggedUser(r.ctx)
	if !usr.IsAdmin {
		return nil, rest.ErrPermissionDenied
	}
	return r.GetAll(r.parseRestOptions(r.ctx, options...))
}

func (r *userRepository) EntityName() string {
	return "user"
}

func (r *userRepository) NewInstance() any {
	return &model.User{}
}

// Save 新建用户，仅管理员可调用。
func (r *userRepository) Save(entity any) (string, error) {
	usr := loggedUser(r.ctx)
	if !usr.IsAdmin {
		return "", rest.ErrPermissionDenied
	}
	u := entity.(*model.User)
	if err := validateUsernameUnique(r, u); err != nil {
		return "", err
	}
	err := r.Put(u)
	if err != nil {
		return "", err
	}
	return u.ID, err
}

// Update 更新用户资料。
//
// 普通用户只能改自己，且须开启 EnableUserEditing；
// 强制回写 IsAdmin=false 与原用户名，防止自行提权或改名。
func (r *userRepository) Update(id string, entity any, _ ...string) error {
	u := entity.(*model.User)
	u.ID = id
	usr := loggedUser(r.ctx)
	if !usr.IsAdmin && usr.ID != u.ID {
		return rest.ErrPermissionDenied
	}
	if !usr.IsAdmin {
		if !conf.Server.EnableUserEditing {
			return rest.ErrPermissionDenied
		}
		u.IsAdmin = false
		u.UserName = usr.UserName
	}

	// Decrypt the user's existing password before validating. This is required otherwise the existing password entered by the user will never match.
	if err := r.decryptPassword(usr); err != nil {
		return err
	}
	if err := validatePasswordChange(u, usr); err != nil {
		return err
	}
	if err := validateUsernameUnique(r, u); err != nil {
		return err
	}
	err := r.Put(u)
	if errors.Is(err, model.ErrNotFound) {
		return rest.ErrNotFound
	}
	return err
}

// validatePasswordChange 校验改密请求。
//
// 管理员改他人密码无需原密码，直接放行。
// 其余情况必须提供正确的当前密码；
// 唯一例外是密码为系统自动生成（带 PasswordAutogenPrefix 前缀）时，
// 用户本就不知道原密码，此时允许直接设置新密码。
func validatePasswordChange(newUser *model.User, logged *model.User) error {
	err := &rest.ValidationError{Errors: map[string]string{}}
	if logged.IsAdmin && newUser.ID != logged.ID {
		return nil
	}
	if newUser.NewPassword == "" {
		if newUser.CurrentPassword == "" {
			return nil
		}
		err.Errors["password"] = "ra.validation.required"
	}

	if !strings.HasPrefix(logged.Password, consts.PasswordAutogenPrefix) {
		if newUser.CurrentPassword == "" {
			err.Errors["currentPassword"] = "ra.validation.required"
		}
		if newUser.CurrentPassword != logged.Password {
			err.Errors["currentPassword"] = "ra.validation.passwordDoesNotMatch"
		}
	}
	if len(err.Errors) > 0 {
		return err
	}
	return nil
}

// validateUsernameUnique 校验用户名唯一。
// 同名记录若就是自己则允许（更新时未改名的情形）。
func validateUsernameUnique(r model.UserRepository, u *model.User) error {
	usr, err := r.FindByUsername(u.UserName)
	if errors.Is(err, model.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if usr.ID != u.ID {
		return &rest.ValidationError{Errors: map[string]string{"userName": "ra.validation.unique"}}
	}
	return nil
}

// Delete 删除用户，仅管理员可调用。
func (r *userRepository) Delete(id string) error {
	usr := loggedUser(r.ctx)
	if !usr.IsAdmin {
		return rest.ErrPermissionDenied
	}
	err := r.delete(Eq{"id": id})
	if errors.Is(err, model.ErrNotFound) {
		return rest.ErrNotFound
	}
	return err
}

// keyTo32Bytes 把任意长度的密钥字符串哈希为 AES 所需的 32 字节。
func keyTo32Bytes(input string) []byte {
	data := sha256.Sum256([]byte(input))
	return data[0:]
}

// initPasswordEncryptionKey 初始化密码加密密钥，并在必要时迁移既有密码。
//
// 未配置自定义密钥时使用内置默认密钥（等同于「未加密」，仅做混淆）。
//
// 配置了自定义密钥时，用密钥的哈希值作为指纹存入 property 表：
//   - 指纹已存在且一致：直接使用；
//   - 指纹已存在但不一致：密钥被改动，所有密码都将无法解密，
//     此处直接报错阻止启动，比让用户全部登录失败更容易排查；
//   - 指纹不存在：说明是首次启用自定义密钥，
//     先用默认密钥解密全部密码，再用新密钥逐个重新加密，最后写入指纹。
func (r *userRepository) initPasswordEncryptionKey() error {
	encKey = keyTo32Bytes(consts.DefaultEncryptionKey)
	if conf.Server.PasswordEncryptionKey == "" {
		return nil
	}

	key := keyTo32Bytes(conf.Server.PasswordEncryptionKey)
	keySum := fmt.Sprintf("%x", sha256.Sum256(key))

	props := NewPropertyRepository(r.ctx, r.db)
	savedKeySum, err := props.Get(consts.PasswordsEncryptedKey)

	// If passwords are already encrypted
	if err == nil {
		if savedKeySum != keySum {
			log.Error("Password Encryption Key changed! Users won't be able to login!")
			return errors.New("passwordEncryptionKey changed")
		}
		encKey = key
		return nil
	}

	// if not, try to re-encrypt all current passwords with new encryption key,
	// assuming they were encrypted with the DefaultEncryptionKey
	sql := r.newSelect().Columns("id", "user_name", "password")
	users := model.Users{}
	err = r.queryAll(sql, &users)
	if err != nil {
		log.Error("Could not encrypt all passwords", err)
		return err
	}
	log.Warn("New PasswordEncryptionKey set. Encrypting all passwords", "numUsers", len(users))
	if err = r.decryptAllPasswords(users); err != nil {
		return err
	}
	encKey = key
	for i := range users {
		u := users[i]
		u.NewPassword = u.Password
		if err := r.encryptPassword(&u); err == nil {
			upd := Update(r.tableName).Set("password", u.NewPassword).Where(Eq{"id": u.ID})
			_, err = r.executeSQL(upd)
			if err != nil {
				log.Error("Password NOT encrypted! This may cause problems!", "user", u.UserName, "id", u.ID, err)
			} else {
				log.Warn("Password encrypted successfully", "user", u.UserName, "id", u.ID)
			}
		}
	}

	err = props.Put(consts.PasswordsEncryptedKey, keySum)
	if err != nil {
		log.Error("Could not flag passwords as encrypted. It will cause login errors", err)
		return err
	}
	return nil
}

// encrypts u.NewPassword
// encryptPassword 就地加密 u.NewPassword。
func (r *userRepository) encryptPassword(u *model.User) error {
	encPassword, err := utils.Encrypt(r.ctx, encKey, u.NewPassword)
	if err != nil {
		log.Error(r.ctx, "Error encrypting user's password", "user", u.UserName, err)
		return err
	}
	u.NewPassword = encPassword
	return nil
}

// decrypts u.Password
// decryptPassword 就地解密 u.Password 为明文。
func (r *userRepository) decryptPassword(u *model.User) error {
	plaintext, err := utils.Decrypt(r.ctx, encKey, u.Password)
	if err != nil {
		log.Error(r.ctx, "Error decrypting user's password", "user", u.UserName, err)
		return err
	}
	u.Password = plaintext
	return nil
}

// decryptAllPasswords 批量解密，供密钥迁移使用。
func (r *userRepository) decryptAllPasswords(users model.Users) error {
	for i := range users {
		if err := r.decryptPassword(&users[i]); err != nil {
			return err
		}
	}
	return nil
}

// Library association methods
// 以下为用户与音乐库的授权关系维护。

// GetUserLibraries 返回用户被授权访问的音乐库，按名称排序。
func (r *userRepository) GetUserLibraries(userID string) (model.Libraries, error) {
	sel := Select("l.*").
		From("library l").
		Join("user_library ul ON l.id = ul.library_id").
		Where(Eq{"ul.user_id": userID}).
		OrderBy("l.name")

	var res model.Libraries
	err := r.queryAll(sel, &res)
	return res, err
}

// SetUserLibraries 全量替换用户的音乐库授权。
// 传入空列表即撤销全部授权。
func (r *userRepository) SetUserLibraries(userID string, libraryIDs []int) error {
	// Remove existing associations
	delSql := Delete("user_library").Where(Eq{"user_id": userID})
	if _, err := r.executeSQL(delSql); err != nil {
		return err
	}

	// Add new associations
	if len(libraryIDs) > 0 {
		insert := Insert("user_library").Columns("user_id", "library_id")
		for _, libID := range libraryIDs {
			insert = insert.Values(userID, libID)
		}
		_, err := r.executeSQL(insert)
		return err
	}
	return nil
}

var _ model.UserRepository = (*userRepository)(nil)
var _ rest.Repository = (*userRepository)(nil)
var _ rest.Persistable = (*userRepository)(nil)
