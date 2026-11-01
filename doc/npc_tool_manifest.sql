-- npc_tool_manifest —— NPC 工具清单（服务端工具描述服务端化的云端目录）
--
-- 说明：本表由 manager 后端启动时的 gorm AutoMigrate 自动创建（manager/backend/database/database.go），
-- 本文件是结构存档与人工建表参考。字段语义与 models.NpcToolManifest / controllers/npc_tool_manifest.go 对齐。
--
-- 语义要点：
--   * 只覆盖/摘除设备侧（游戏客户端）已上报的 MCP 工具，不新增工具；
--     客户端 can_offer 仍是工具可用性的第一裁决，本表只做目录级调整。
--   * agent_id='' 的条目是全局默认清单；agent 专属条目优先于全局条目（按 tool_name 对齐）。
--   * enabled=0 的条目在主服务工具聚合时被摘除（云端总开关）。
--   * description/input_schema 为空串表示不覆盖对应字段。
--
-- 主服务消费路径：GET /api/internal/npc-tools?agent_id=X（InternalServiceAuth 保护），
-- 30s TTL 缓存，见 internal/domain/mcp/tool_manifest.go 与 toolmanifest 子包。

CREATE TABLE IF NOT EXISTS `npc_tool_manifest` (
  `id`          bigint unsigned NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `agent_id`    varchar(64)     NOT NULL DEFAULT ''     COMMENT '智能体ID（agents.id 的字符串形态）；空串=全局默认清单',
  `tool_name`   varchar(128)    NOT NULL DEFAULT ''     COMMENT 'MCP工具名，与设备侧上报的工具名严格一致（如 npc_trade）',
  `description` longtext        NULL                    COMMENT '覆盖工具描述（LLM 据此判定调用时机）；空串=不覆盖',
  `input_schema` longtext       NULL                    COMMENT '覆盖工具入参 schema（openapi3 JSON 对象文本）；空串=不覆盖',
  `enabled`     tinyint(1)      NOT NULL DEFAULT 1      COMMENT '是否启用；0=聚合时从工具列表摘除（云端总开关）',
  `remark`      varchar(255)    NULL DEFAULT ''         COMMENT '运营备注（改动原因等，仅人工查阅）',
  `created_at`  datetime(3)     NULL DEFAULT NULL       COMMENT '创建时间',
  `updated_at`  datetime(3)     NULL DEFAULT NULL       COMMENT '最近更新时间',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_npc_tool_agent_tool` (`agent_id`, `tool_name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='NPC工具清单（服务端工具描述服务端化的云端目录）';

-- 种子示例（可选执行，验证用）：全局默认——所有 agent 的 npc_trade 描述统一收口
-- INSERT INTO `npc_tool_manifest` (`agent_id`, `tool_name`, `description`, `input_schema`, `enabled`, `remark`)
-- VALUES ('', 'npc_trade', '与该NPC进行物品买卖（浏览商品、购买、出售）。当玩家表达想买东西、卖东西、看看货品时调用。', '', 1, '阶段一验证种子');
