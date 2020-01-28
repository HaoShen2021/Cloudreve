package model

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"github.com/HFO4/cloudreve/pkg/util"
	"github.com/jinzhu/gorm"
	"github.com/pkg/errors"
	"strings"
	"time"
)

const (
	// Active 账户正常状态
	Active = iota
	// NotActivicated 未激活
	NotActivicated
	// Baned 被封禁
	Baned
)

// User 用户模型
type User struct {
	// 表字段
	gorm.Model
	Email         string `gorm:"type:varchar(100);unique_index"`
	Nick          string `gorm:"size:50"`
	Password      string `json:"-"`
	Status        int
	GroupID       uint
	PrimaryGroup  int
	ActivationKey string `json:"-"`
	Storage       uint64
	LastNotify    *time.Time
	OpenID        string `json:"-"`
	TwoFactor     string `json:"-"`
	Delay         int
	Avatar        string
	Options       string `json:"-",gorm:"size:4096"`
	Authn         string `gorm:"size:8192"`
	Score         int

	// 关联模型
	Group  Group  `gorm:"association_autoupdate:false"`
	Policy Policy `gorm:"PRELOAD:false,association_autoupdate:false"`

	// 数据库忽略字段
	OptionsSerialized UserOption `gorm:"-"`
}

// UserOption 用户个性化配置字段
type UserOption struct {
	ProfileOn       int    `json:"profile_on"`
	PreferredPolicy uint   `json:"preferred_policy"`
	WebDAVKey       string `json:"webdav_key"`
	PreferredTheme  string `json:"preferred_theme"`
}

// Root 获取用户的根目录
func (user *User) Root() (*Folder, error) {
	var folder Folder
	err := DB.Where("parent_id = 0 AND owner_id = ?", user.ID).First(&folder).Error
	return &folder, err
}

// DeductionStorage 减少用户已用容量
func (user *User) DeductionStorage(size uint64) bool {
	if size == 0 {
		return true
	}
	if size <= user.Storage {
		user.Storage -= size
		DB.Model(user).UpdateColumn("storage", gorm.Expr("storage - ?", size))
		return true
	}
	// 如果要减少的容量超出已用容量，则设为零
	user.Storage = 0
	DB.Model(user).UpdateColumn("storage", 0)

	return false
}

// IncreaseStorage 检查并增加用户已用容量
func (user *User) IncreaseStorage(size uint64) bool {
	if size == 0 {
		return true
	}
	if size <= user.GetRemainingCapacity() {
		user.Storage += size
		DB.Model(user).UpdateColumn("storage", gorm.Expr("storage + ?", size))
		return true
	}
	return false
}

// PayScore 扣除积分，返回是否成功
// todo 测试
func (user *User) PayScore(score int) bool {
	if score == 0 {
		return true
	}
	if score <= user.Score {
		user.Score -= score
		DB.Model(user).UpdateColumn("score", gorm.Expr("score - ?", score))
		return true
	}
	return false
}

// IncreaseStorageWithoutCheck 忽略可用容量，增加用户已用容量
func (user *User) IncreaseStorageWithoutCheck(size uint64) {
	if size == 0 {
		return
	}
	user.Storage += size
	DB.Model(user).UpdateColumn("storage", gorm.Expr("storage + ?", size))

}

// GetRemainingCapacity 获取剩余配额
func (user *User) GetRemainingCapacity() uint64 {
	total := user.Group.MaxStorage + user.GetAvailablePackSize()
	if total <= user.Storage {
		return 0
	}
	return total - user.Storage
}

// GetPolicyID 获取用户当前的存储策略ID
func (user *User) GetPolicyID() uint {
	// 用户未指定时，返回可用的第一个
	if user.OptionsSerialized.PreferredPolicy == 0 {
		if len(user.Group.PolicyList) != 0 {
			return user.Group.PolicyList[0]
		}
		return 1
	}
	// 用户指定时，先检查是否为可用策略列表中的值
	if util.ContainsUint(user.Group.PolicyList, user.OptionsSerialized.PreferredPolicy) {
		return user.OptionsSerialized.PreferredPolicy
	}
	// 不可用时，返回第一个
	if len(user.Group.PolicyList) != 0 {
		return user.Group.PolicyList[0]
	}
	return 1

}

// GetUserByID 用ID获取用户
func GetUserByID(ID interface{}) (User, error) {
	var user User
	result := DB.Set("gorm:auto_preload", true).First(&user, ID)
	return user, result.Error
}

// GetUserByEmail 用Email获取用户
func GetUserByEmail(email string) (User, error) {
	var user User
	result := DB.Set("gorm:auto_preload", true).Where("email = ?", email).First(&user)
	return user, result.Error
}

// NewUser 返回一个新的空 User
func NewUser() User {
	options := UserOption{
		ProfileOn: 1,
	}
	return User{
		Avatar:            "default",
		OptionsSerialized: options,
	}
}

// BeforeSave Save用户前的钩子
func (user *User) BeforeSave() (err error) {
	err = user.SerializeOptions()
	return err
}

// AfterCreate 创建用户后的钩子
func (user *User) AfterCreate(tx *gorm.DB) (err error) {
	// 创建用户的默认根目录
	defaultFolder := &Folder{
		Name:    "/",
		OwnerID: user.ID,
	}
	tx.Create(defaultFolder)
	return err
}

// AfterFind 找到用户后的钩子
func (user *User) AfterFind() (err error) {
	// 解析用户设置到OptionsSerialized
	if user.Options != "" {
		err = json.Unmarshal([]byte(user.Options), &user.OptionsSerialized)
	}

	// 预加载存储策略
	user.Policy, _ = GetPolicyByID(user.GetPolicyID())
	return err
}

//SerializeOptions 将序列后的Option写入到数据库字段
func (user *User) SerializeOptions() (err error) {
	optionsValue, err := json.Marshal(&user.OptionsSerialized)
	user.Options = string(optionsValue)
	return err
}

// CheckPassword 根据明文校验密码
func (user *User) CheckPassword(password string) (bool, error) {

	// 根据存储密码拆分为 Salt 和 Digest
	passwordStore := strings.Split(user.Password, ":")
	if len(passwordStore) != 2 {
		return false, errors.New("Unknown password type")
	}

	// todo 兼容V2/V1密码
	//计算 Salt 和密码组合的SHA1摘要
	hash := sha1.New()
	_, err := hash.Write([]byte(password + passwordStore[0]))
	bs := hex.EncodeToString(hash.Sum(nil))
	if err != nil {
		return false, err
	}

	return bs == passwordStore[1], nil
}

// SetPassword 根据给定明文设定 User 的 Password 字段
func (user *User) SetPassword(password string) error {
	//生成16位 Salt
	salt := util.RandStringRunes(16)

	//计算 Salt 和密码组合的SHA1摘要
	hash := sha1.New()
	_, err := hash.Write([]byte(password + salt))
	bs := hex.EncodeToString(hash.Sum(nil))

	if err != nil {
		return err
	}

	//存储 Salt 值和摘要， ":"分割
	user.Password = salt + ":" + string(bs)
	return nil
}

// NewAnonymousUser 返回一个匿名用户
// TODO 测试
func NewAnonymousUser() *User {
	user := User{}
	user.Group, _ = GetGroupByID(3)
	return &user
}

// IsAnonymous 返回是否为未登录用户
// TODO 测试
func (user *User) IsAnonymous() bool {
	return user.ID == 0
}
