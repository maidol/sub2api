-- proxies: 代理池 slot 共享设置（仅 managed_by = 'pool' 的行有意义）
ALTER TABLE proxies ADD COLUMN IF NOT EXISTS pool_shareable boolean NOT NULL DEFAULT false;
ALTER TABLE proxies ADD COLUMN IF NOT EXISTS pool_share_max integer NOT NULL DEFAULT 0;
