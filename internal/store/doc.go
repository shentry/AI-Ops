// Package store 是唯一的数据库边界。
//
// 只有本包可以 import gorm.io/gorm。表结构在 migrations/*.sql，手工执行；
// Open 不做 AutoMigrate。
//
// # 文件划分
//
// 全部方法挂在同一个 *DB 上，按告警处理链路的阶段切分文件：
//
//	db.go            连接、分页上界、落库前截断
//	models.go        表结构（对应 migrations/001_init.sql）
//	rawevent.go      摄入：落原文、取 pending、按指纹去重应用
//	incident.go      归并事务与 incident 查询
//	event.go         incident_event 审计流
//	problem.go       incident_problem 问题面板
//	agentrun.go      诊断队列：入队、CAS 领取、超时重排、终态
//	runstep.go       逐步审计与 CompleteRun 单事务收尾
//	approval.go      审批单生命周期
//	execution.go     执行闸门：executing 的 CAS 推进与崩溃恢复
//	conversation.go  Incident 对话队列
//	integration.go   飞书回调幂等与消息绑定
//	memory.go        故障记忆与执行历史
//	llmmodel.go      全局模型选择
//
// 测试按同一套域划分，共享脚手架在 helpers_test.go。全部测试打真 MySQL，
// 默认 go test ./... 会整包 skip，要真跑必须显式给库：
//
//	TEST_MYSQL_DSN="$MYSQL_DSN" go test ./internal/store
//
// # 为什么不拆成子包
//
// 因为事务跨域。FinishApprovalExecution 在一笔事务里同时写 approval、
// incident_event 和 incident_problem；CompleteRun 在一笔事务里写 agent_run、
// agent_run_step、incident_event 和 incident_problem。这些跨域收尾共享的是同一个
// *gorm.DB 事务句柄 —— 拆包就得把它导出，gorm 随之泄漏到 store 之外，
// 上面第一条不变量当场失效。
//
// 需要窄接口的调用方自己在消费侧声明（例如 diagnose.verifyStore、
// feishu.CallbackStore），不要在这里预先切分。
package store
