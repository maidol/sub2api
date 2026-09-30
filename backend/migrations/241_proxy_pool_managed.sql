-- proxies: 代理池托管标记（空 = 手工代理；pool = 从代理池提供方租来的）与提供方租约 ID
ALTER TABLE proxies ADD COLUMN IF NOT EXISTS managed_by varchar(20) NOT NULL DEFAULT '';
ALTER TABLE proxies ADD COLUMN IF NOT EXISTS external_ref varchar(100) NOT NULL DEFAULT '';
