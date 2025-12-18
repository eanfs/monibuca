package config

type DB struct {
	DSN    string `default:"m7s.db" desc:"数据库文件路径"`
	DBType string `default:"sqlite" desc:"数据库类型"`
}
