I use this program to monitor IMAP mailboxes for new messages.

Specifically, I run it in a cronjob and connect to my various Gmail accounts
and tell it to watch the Spam folders. When there are new messages in a Spam
folder, I receive a notice. This way I can monitor for false positives without
downloading the messages or logging into Gmail's interface.

It logs into an IMAP server, selects a mailbox, and fetches information about
all of the messages in the mailbox. It checks each one whether it is in a
PostgreSQL database, and inserting it into the database and notifying if it is
not. I do this by storing the message-id and IMAP internal date.
