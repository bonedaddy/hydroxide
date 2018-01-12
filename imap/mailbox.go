package imap

import (
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap"

	"github.com/emersion/hydroxide/imap/database"
	"github.com/emersion/hydroxide/protonmail"
)

const delimiter = "/"

type mailbox struct {
	name string
	label string
	flags []string

	u *user
	db *database.Mailbox

	initialized bool
	initializedLock sync.Mutex

	total, unread int
}

func (mbox *mailbox) Name() string {
	return mbox.name
}

func (mbox *mailbox) Info() (*imap.MailboxInfo, error) {
	return &imap.MailboxInfo{
		Attributes: append(mbox.flags, imap.NoInferiorsAttr),
		Delimiter: delimiter,
		Name: mbox.name,
	}, nil
}

func (mbox *mailbox) Status(items []imap.StatusItem) (*imap.MailboxStatus, error) {
	status := imap.NewMailboxStatus(mbox.name, items)
	status.Flags = mbox.flags
	status.PermanentFlags = []string{imap.SeenFlag, imap.AnsweredFlag, imap.FlaggedFlag, imap.DeletedFlag, imap.DraftFlag}
	status.UnseenSeqNum = 0 // TODO

	for _, name := range items {
		switch name {
		case imap.StatusMessages:
			status.Messages = uint32(mbox.total)
		case imap.StatusUidNext:
			uidNext, err := mbox.db.UidNext()
			if err != nil {
				return nil, err
			}
			status.UidNext = uidNext
		case imap.StatusUidValidity:
			status.UidValidity = 1
		case imap.StatusRecent:
			status.Recent = 0
		case imap.StatusUnseen:
			status.Unseen = uint32(mbox.unread)
		}
	}

	return status, nil
}

func (mbox *mailbox) SetSubscribed(subscribed bool) error {
	return errNotYetImplemented // TODO
}

func (mbox *mailbox) Check() error {
	return nil
}

func (mbox *mailbox) sync() error {
	log.Printf("Synchronizing mailbox %v...", mbox.name)

	filter := &protonmail.MessageFilter{
		PageSize: 150,
		Label: mbox.label,
		Sort: "ID",
		Asc: true,
	}

	total := -1
	for {
		offset := filter.PageSize * filter.Page
		if total >= 0 && offset > total {
			break
		}

		var page []*protonmail.Message
		var err error
		total, page, err = mbox.u.c.ListMessages(filter)
		if err != nil {
			return err
		}

		if err := mbox.db.Sync(page); err != nil {
			return err
		}

		filter.Page++
	}

	log.Printf("Synchronizing mailbox %v: done.", mbox.name)

	return nil
}

func (mbox *mailbox) init() error {
	mbox.initializedLock.Lock()
	defer mbox.initializedLock.Unlock()

	if mbox.initialized {
		return nil
	}

	// TODO: sync only the first time
	if err := mbox.sync(); err != nil {
		return err
	}

	mbox.initialized = true
	return nil
}

func (mbox *mailbox) reset() error {
	mbox.initializedLock.Lock()
	defer mbox.initializedLock.Unlock()

	mbox.initialized = false

	return mbox.db.Reset()
}

func (mbox *mailbox) fetchMessage(isUid bool, id uint32, items []imap.FetchItem) (*imap.Message, error) {
	var apiID string
	var err error
	if isUid {
		apiID, err = mbox.db.FromUid(id)
	} else {
		apiID, err = mbox.db.FromSeqNum(id)
	}
	if err != nil {
		return nil, err
	}

	seqNum, uid, err := mbox.db.FromApiID(apiID)
	if err != nil {
		return nil, err
	}

	msg, err := mbox.u.db.Message(apiID)
	if err != nil {
		return nil, err
	}

	fetched := imap.NewMessage(seqNum, items)
	for _, item := range items {
		switch item {
		case imap.FetchEnvelope:
			fetched.Envelope = fetchEnvelope(msg)
		case imap.FetchBody, imap.FetchBodyStructure:
			bs, err := mbox.fetchBodyStructure(msg, item == imap.FetchBodyStructure)
			if err != nil {
				return nil, err
			}
			fetched.BodyStructure = bs
		case imap.FetchFlags:
			fetched.Flags = fetchFlags(msg)
		case imap.FetchInternalDate:
			fetched.InternalDate = time.Unix(msg.Time, 0)
		case imap.FetchRFC822Size:
			fetched.Size = uint32(msg.Size)
		case imap.FetchUid:
			fetched.Uid = uid
		default:
			section, err := imap.ParseBodySectionName(item)
			if err != nil {
				break
			}

			l, err := mbox.fetchBodySection(msg, section)
			if err != nil {
				return nil, err
			}
			fetched.Body[section] = l
		}
	}

	return fetched, nil
}

func (mbox *mailbox) ListMessages(uid bool, seqSet *imap.SeqSet, items []imap.FetchItem, ch chan<- *imap.Message) error {
	defer close(ch)

	if err := mbox.init(); err != nil {
		return err
	}

	for _, seq := range seqSet.Set {
		start := seq.Start
		if start == 0 {
			start = 1
		}

		stop := seq.Stop
		if stop == 0 {
			if uid {
				uidNext, err := mbox.db.UidNext()
				if err != nil {
					return err
				}
				stop = uidNext - 1
			} else {
				stop = uint32(mbox.total)
			}
		}

		for i := start; i <= stop; i++ {
			msg, err := mbox.fetchMessage(uid, i, items)
			if err == database.ErrNotFound {
				continue
			} else if err != nil {
				return err
			}
			if msg != nil {
				ch <- msg
			}
		}
	}

	return nil
}

func matchString(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

func (mbox *mailbox) SearchMessages(isUID bool, c *imap.SearchCriteria) ([]uint32, error) {
	if err := mbox.init(); err != nil {
		return nil, err
	}

	// TODO: c.Not, c.Or
	if c.Not != nil || c.Or != nil {
		return nil, errors.New("search queries with NOT or OR clauses or not yet implemented")
	}

	var results []uint32
	err := mbox.db.ForEach(func(seqNum, uid uint32, apiID string) error {
		if c.SeqNum != nil && !c.SeqNum.Contains(seqNum) {
			return nil
		}
		if c.Uid != nil && !c.Uid.Contains(uid) {
			return nil
		}

		// TODO: fetch message from local DB only if needed
		msg, err := mbox.u.db.Message(apiID)
		if err != nil {
			return err
		}

		flags := make(map[string]bool)
		for _, flag := range fetchFlags(msg) {
			flags[flag] = true
		}
		for _, f := range c.WithFlags {
			if !flags[f] {
				return nil
			}
		}
		for _, f := range c.WithoutFlags {
			if flags[f] {
				return nil
			}
		}

		date := time.Unix(msg.Time, 0).Round(24 * time.Hour)
		if !c.Since.IsZero() && !date.After(c.Since) {
			return nil
		}
		if !c.Before.IsZero() && !date.Before(c.Before) {
			return nil
		}
		// TODO: this date should be from the Date MIME header
		if !c.SentBefore.IsZero() && !date.Before(c.SentBefore) {
			return nil
		}
		if !c.SentSince.IsZero() && !date.After(c.SentSince) {
			return nil
		}

		h := messageHeader(msg)
		for key, wantValues := range c.Header {
			values, ok := h[key]
			for _, wantValue := range wantValues {
				if wantValue == "" && !ok {
					return nil
				}
				if wantValue != "" {
					ok := false
					for _, v := range values {
						if matchString(v, wantValue) {
							ok = true
							break
						}
					}
					if !ok {
						return nil
					}
				}
			}
		}

		// TODO: c.Body, c.Text

		if c.Larger > 0 && uint32(msg.Size) < c.Larger {
			return nil
		}
		if c.Smaller > 0 && uint32(msg.Size) > c.Smaller {
			return nil
		}

		if isUID {
			results = append(results, uid)
		} else {
			results = append(results, seqNum)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

func (mbox *mailbox) CreateMessage(flags []string, date time.Time, body imap.Literal) error {
	return errNotYetImplemented // TODO
}

func (mbox *mailbox) UpdateMessagesFlags(uid bool, seqSet *imap.SeqSet, operation imap.FlagsOp, flags []string) error {
	return errNotYetImplemented // TODO
}

func (mbox *mailbox) CopyMessages(uid bool, seqSet *imap.SeqSet, dest string) error {
	return errNotYetImplemented // TODO
}

func (mbox *mailbox) Expunge() error {
	return errNotYetImplemented // TODO
}
