-- A group may post to a person: destination U… is a workspace member, and the
-- mirror DMs them as the bot (chat.postMessage opens the conversation from
-- the user id). The CHECK widens in place; every existing row already
-- satisfies the new shape.
ALTER TABLE slack_groups DROP CONSTRAINT slack_groups_destination_check;
ALTER TABLE slack_groups ADD CONSTRAINT slack_groups_destination_check
  CHECK (destination = 'dm' OR destination ~ '^[CGU][A-Z0-9]{6,20}$');
