CREATE TABLE "analytics_export_watermarks" (
	"stream" text PRIMARY KEY NOT NULL,
	"confirmed_at" timestamp with time zone NOT NULL,
	"updated_at" timestamp with time zone DEFAULT now() NOT NULL
);
--> statement-breakpoint
ALTER TABLE "earn_yield_push_state" ADD COLUMN "recent_pushed_at" timestamp with time zone[] DEFAULT '{}'::timestamptz[] NOT NULL;--> statement-breakpoint
ALTER TABLE "earn_yield_push_state" ADD COLUMN "rule_version" smallint DEFAULT 1 NOT NULL;