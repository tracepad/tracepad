-- Who holds and who finished a queue item, for a signed-in reviewer: the
-- account's id (spec 048 #15), where it used to be the display string the desk
-- sent. A name is neither unique nor stable, and the desk that sent it also
-- sent an email when the account had no name. The free-text `claimed_by` and
-- `completed_by` stay, for a program working a queue with a key: it has a name
-- and no account. A row holds one or the other, never both.
--
-- Plain ids with no foreign key, as the score's author (#2): deleting an
-- account takes nothing else with it (spec 028 #12).
ALTER TABLE annotation_items ADD COLUMN claimed_by_account TEXT
    CHECK (claimed_by_account IS NULL OR claimed_by IS NULL);
ALTER TABLE annotation_items ADD COLUMN completed_by_account TEXT
    CHECK (completed_by_account IS NULL OR completed_by IS NULL);
