-- Holds reserve part of an account's available balance until they are
-- captured, voided or expire.
CREATE TABLE holds (
    id                     BIGINT       NOT NULL AUTO_INCREMENT,
    account_id             BIGINT       NOT NULL,
    destination_account_id BIGINT       NOT NULL,
    currency               CHAR(3)      NOT NULL,
    amount                 BIGINT       NOT NULL,
    captured_amount        BIGINT       NOT NULL DEFAULT 0,
    state                  ENUM('active', 'partially_captured', 'captured', 'voided', 'expired') NOT NULL,
    description            VARCHAR(255) NOT NULL DEFAULT '',
    external_ref           VARCHAR(128) NULL,
    expires_at             DATETIME(6)  NOT NULL,
    created_at             DATETIME(6)  NOT NULL,
    updated_at             DATETIME(6)  NOT NULL,
    PRIMARY KEY (id),
    KEY idx_holds_state_expires (state, expires_at),
    KEY idx_holds_account (account_id, state),
    CONSTRAINT fk_holds_account FOREIGN KEY (account_id) REFERENCES accounts (id),
    CONSTRAINT fk_holds_destination FOREIGN KEY (destination_account_id) REFERENCES accounts (id),
    CONSTRAINT chk_holds_amount CHECK (amount > 0 AND captured_amount >= 0 AND captured_amount <= amount)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_bin;

ALTER TABLE journal_entries
    ADD CONSTRAINT fk_entries_hold FOREIGN KEY (hold_id) REFERENCES holds (id);
