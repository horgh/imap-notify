//
// This program connects to an IMAP account and retrieves information about
// messages in a specified mailbox. It checks each message for whether it was
// previously seen, and notifies if not.
//
// My purpose is to be able to watch my Gmail spam folders without having to
// download the messages or manually monitor them. Instead I will receive
// notifications to be able to decide if there is a false positive I care
// about.
//
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"mime"
	"net"
	"os"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"golang.org/x/net/html/charset"

	_ "github.com/lib/pq"
)

// Args holds command line arguments.
type Args struct {
	Host         string
	Port         int
	User         string
	PasswordFile string
	Mailbox      string

	DBHost string
	DBPort int
	DBUser string
	DBPass string
	DBName string

	Verbose bool
}

// Message holds information about a message in the IMAP mailbox.
type Message struct {
	MessageID string
	From      []string
	Subject   string
	// Date the message was received by server. Not header date.
	InternalDate time.Time
}

// DBMessage holds information about a message from the database.
type DBMessage struct {
	ID            int
	MessageID     string
	Subject       string
	FromAddresses string
	InternalDate  time.Time
	CreateTime    time.Time
}

func main() {
	log.SetFlags(0)

	args, err := getArgs()
	if err != nil {
		flag.PrintDefaults()
		log.Fatal(err)
	}

	pass, err := readFile(args.PasswordFile)
	if err != nil {
		log.Fatalf("Unable to retrieve password from file: %s: %s",
			args.PasswordFile, err)
	}

	messages, err := fetchMessages(args.Host, args.Port, args.User, pass,
		args.Mailbox, args.Verbose)
	if err != nil {
		log.Fatalf("Unable to fetch messages: %s", err)
	}

	db, err := connectToDB(args.DBHost, args.DBUser, args.DBPass, args.DBName,
		args.DBPort)
	if err != nil {
		log.Fatalf("Unable to connect to the database: %s", err)
	}
	defer func() {
		err := db.Close()
		if err != nil {
			log.Printf("Error closing database connection: %s", err)
		}
	}()

	err = storeAndReportMessages(db, messages, args.Verbose)
	if err != nil {
		log.Fatalf("Unable to report mesages: %s", err)
	}
}

func getArgs() (*Args, error) {
	host := flag.String("host", "", "IMAP host.")
	port := flag.Int("port", 993, "IMAP port.")
	user := flag.String("user", "", "IMAP username.")
	passwordFile := flag.String("password-file", "", "File containing the IMAP password.")
	mailbox := flag.String("mailbox", "", "IMAP mailbox.")

	dbHost := flag.String("db-host", "127.0.0.1", "Database host.")
	dbPort := flag.Int("db-port", 5432, "Database port.")
	dbUser := flag.String("db-user", "", "Database username.")
	dbPass := flag.String("db-pass", "", "Database password.")
	dbName := flag.String("db-name", "", "Database name.")

	verbose := flag.Bool("verbose", false, "Toggle verbose output.")

	flag.Parse()

	if len(*host) == 0 {
		return nil, fmt.Errorf("You must provide an IMAP host.")
	}

	if len(*user) == 0 {
		return nil, fmt.Errorf("You must provide an IMAP username.")
	}

	if len(*passwordFile) == 0 {
		return nil, fmt.Errorf("You must provide an IMAP password file.")
	}

	if len(*mailbox) == 0 {
		return nil, fmt.Errorf("You must provide an IMAP mailbox.")
	}

	if len(*dbHost) == 0 {
		return nil, fmt.Errorf("You must provide a database host.")
	}

	if len(*dbUser) == 0 {
		return nil, fmt.Errorf("You must provide a database username.")
	}

	if len(*dbPass) == 0 {
		return nil, fmt.Errorf("You must provide a database password.")
	}

	if len(*dbName) == 0 {
		return nil, fmt.Errorf("You must provide a database name.")
	}

	return &Args{
		Host:         *host,
		Port:         *port,
		User:         *user,
		PasswordFile: *passwordFile,
		Mailbox:      *mailbox,

		DBHost: *dbHost,
		DBPort: *dbPort,
		DBUser: *dbUser,
		DBPass: *dbPass,
		DBName: *dbName,

		Verbose: *verbose,
	}, nil
}

func readFile(file string) (string, error) {
	fh, err := os.Open(file)
	if err != nil {
		return "", err
	}

	contents, err := ioutil.ReadAll(fh)
	if err != nil {
		_ = fh.Close()
		return "", fmt.Errorf("Unable to read from file: %s", err)
	}

	err = fh.Close()
	if err != nil {
		return "", fmt.Errorf("Close: %s", err)
	}

	s := strings.TrimSpace(string(contents))

	if len(s) == 0 {
		return "", fmt.Errorf("No contents found")
	}

	return s, nil
}

func fetchMessages(host string, port int, user, pass,
	mailbox string, verbose bool) ([]*Message, error) {

	address := fmt.Sprintf("%s:%d", host, port)

	if verbose {
		log.Printf("Connecting to %s...", address)
	}

	timeout := 30 * time.Second

	dialer := &net.Dialer{Timeout: timeout}
	client, err := client.DialWithDialerTLS(dialer, address, nil)
	if err != nil {
		return nil, fmt.Errorf("Unable to connect to IMAP server: %s", err)
	}

	client.Timeout = timeout

	if verbose {
		log.Printf("Connected to %s", address)
	}

	defer func() {
		if verbose {
			log.Printf("Logging out...")
		}

		err := client.Logout()
		if err != nil {
			log.Printf("Error closing client connection: %s", err)
			return
		}

		if verbose {
			log.Printf("Logged out.")
		}
	}()

	if verbose {
		log.Printf("Logging in as %s...", user)
	}

	err = client.Login(user, pass)
	if err != nil {
		if verbose {
			log.Printf("Unable to login: %s", err)
		}
		return nil, fmt.Errorf("Unable to login to IMAP: %s", err)
	}

	if verbose {
		log.Printf("Logged in as %s", user)
	}

	mbox, err := client.Select(mailbox, true)
	if err != nil {
		return nil, fmt.Errorf("Unable to select mailbox: %s: %s", mailbox, err)
	}

	if verbose {
		log.Printf("There are %d messages in the mailbox.", mbox.Messages)
	}

	// NewSeqSet will return an error used this way apparently... Ignore it, we
	// expect it. We fix it when we use AddRange().
	seqset, _ := imap.NewSeqSet("")

	seqset.AddRange(1, mbox.Messages)

	imapMessages := make(chan *imap.Message)
	done := make(chan error, 1)
	go func() {
		done <- client.Fetch(seqset, []string{imap.EnvelopeMsgAttr,
			imap.InternalDateMsgAttr}, imapMessages)
	}()

	messages := []*Message{}

	for msg := range imapMessages {
		message := &Message{
			MessageID:    msg.Envelope.MessageId,
			Subject:      msg.Envelope.Subject,
			From:         []string{},
			InternalDate: msg.InternalDate,
		}

		for _, address := range msg.Envelope.From {
			message.From = append(message.From, fmt.Sprintf("%s <%s@%s>",
				address.PersonalName, address.MailboxName, address.HostName))
		}

		messages = append(messages, message)
	}

	err = <-done
	if err != nil {
		return nil, fmt.Errorf("Problem fetching messages: %s", err)
	}

	return messages, nil
}

func connectToDB(host, user, pass, name string, port int) (*sql.DB, error) {
	dsn := fmt.Sprintf("user=%s password=%s dbname=%s host=%s port=%d connect_timeout=10",
		user, pass, name, host, port)

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("Failed to connect to database: %s", err)
	}

	return db, nil
}

func storeAndReportMessages(db *sql.DB, messages []*Message,
	verbose bool) error {
	for _, message := range messages {
		// I expect there will always be a message-id. I believe Gmail adds one if
		// a message comes in without it. But check.
		if len(message.MessageID) == 0 {
			log.Printf("WARNING: Message has no message-id. %s", message)
			continue
		}

		// See if the message is already in the database.
		// If it is, proceed to the next.
		// If it's not, record it, and notify.
		dbMessages, err := dbGetMessage(db, message.MessageID, message.InternalDate)
		if err != nil {
			return fmt.Errorf("Unable to retrieve messages from database: %s", err)
		}

		if len(dbMessages) == 1 {
			if verbose {
				log.Printf("Message already seen: %s", message)
				log.Printf("In database it is: %s", dbMessages[0])
			}
			continue
		}

		if len(dbMessages) > 1 {
			log.Printf("WARNING: Multiple matching messages in the database! %s",
				message)
			continue
		}

		err = dbInsertMessage(db, message)
		if err != nil {
			return fmt.Errorf("Unable to insert message: %s: %s", message, err)
		}

		err = outputMessage(message)
		if err != nil {
			return fmt.Errorf("Unable to output message: %s: %s", message, err)
		}
	}

	return nil
}

func dbGetMessage(db *sql.DB, messageID string,
	internalDate time.Time) ([]*DBMessage, error) {
	// Rationale for using internal date: It is possible for message ids to not
	// be unique (but they should be).

	query := `
	SELECT id, message_id, subject, from_addresses, internal_date, create_time
	FROM imap_notify
	WHERE message_id = $1 AND internal_date = $2
	`

	rows, err := db.Query(query, messageID, internalDate)
	if err != nil {
		return nil, fmt.Errorf("Unable to query: %s", err)
	}

	messages := []*DBMessage{}

	for rows.Next() {
		message := &DBMessage{}

		err := rows.Scan(&message.ID, &message.MessageID, &message.Subject,
			&message.FromAddresses, &message.InternalDate, &message.CreateTime)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("Unable to scan row: %s", err)
		}

		messages = append(messages, message)
	}

	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("Problem selecting from database: %s", err)
	}

	return messages, nil
}

func dbInsertMessage(db *sql.DB, message *Message) error {
	query := `
	INSERT INTO imap_notify
	(message_id, subject, from_addresses, internal_date)
	VALUES($1, $2, $3, $4)
	`

	fromAddresses := strings.Join(message.From, ", ")

	_, err := db.Exec(query, message.MessageID, message.Subject, fromAddresses,
		message.InternalDate)
	if err != nil {
		return fmt.Errorf("Unable to insert: %s", err)
	}

	return nil
}

func outputMessage(message *Message) error {
	decoder := &mime.WordDecoder{
		CharsetReader: charset.NewReaderLabel,
	}

	subject, err := decoder.DecodeHeader(message.Subject)
	if err != nil {
		log.Printf("Unable to decode subject: %s", err)
		subject = message.Subject
	}

	log.Printf("----------")
	log.Printf("")
	log.Printf("Subject: %s", subject)
	for _, fromHeader := range message.From {
		from, err := decoder.DecodeHeader(fromHeader)
		if err != nil {
			log.Printf("Unable to decode from: %s: %s", err, fromHeader)
			from = fromHeader
		}

		log.Printf("From: %s", from)
	}
	log.Printf("")

	return nil
}

func (m *Message) String() string {
	return fmt.Sprintf("Message-ID: %s Subject: %s Time: %s From: %s",
		m.MessageID, m.Subject, m.InternalDate, strings.Join(m.From, ", "))
}

func (m *DBMessage) String() string {
	return fmt.Sprintf("ID: %d Message-ID: %s Subject: %s Time: %s From: %s Create Time: %s",
		m.ID, m.MessageID, m.Subject, m.InternalDate, m.FromAddresses, m.CreateTime)
}
