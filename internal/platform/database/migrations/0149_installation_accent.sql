ALTER TABLE olp.installation
    ADD COLUMN accent text NOT NULL DEFAULT '' CHECK (accent = '' OR accent ~ '^#[0-9a-f]{6}$');
