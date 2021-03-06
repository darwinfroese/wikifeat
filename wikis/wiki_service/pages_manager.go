/*
 *  Licensed to Wikifeat under one or more contributor license agreements.
 *  See the LICENSE.txt file distributed with this work for additional information
 *  regarding copyright ownership.
 *
 *  Redistribution and use in source and binary forms, with or without
 *  modification, are permitted provided that the following conditions are met:
 *
 *  * Redistributions of source code must retain the above copyright notice,
 *  this list of conditions and the following disclaimer.
 *  * Redistributions in binary form must reproduce the above copyright
 *  notice, this list of conditions and the following disclaimer in the
 *  documentation and/or other materials provided with the distribution.
 *  * Neither the name of Wikifeat nor the names of its contributors may be used
 *  to endorse or promote products derived from this software without
 *  specific prior written permission.
 *
 *  THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
 *  AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
 *  IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE
 *  ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT OWNER OR CONTRIBUTORS BE
 *  LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR
 *  CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF
 *  SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS
 *  INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN
 *  CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE)
 *  ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE
 *  POSSIBILITY OF SUCH DAMAGE.
 */

package wiki_service

import (
	"errors"
	"github.com/microcosm-cc/bluemonday"
	"github.com/rhinoman/go-commonmark"
	. "github.com/rhinoman/wikifeat/common/database"
	. "github.com/rhinoman/wikifeat/common/entities"
	"github.com/rhinoman/wikifeat/common/util"
	"github.com/rhinoman/wikifeat/wikis/wiki_service/wikit"
	"log"
	"regexp"
)

type PageManager struct{}

type Breadcrumb struct {
	Name   string `json:"name"`
	PageId string `json:"pageId"`
	WikiId string `json:"wikiId"`
	Parent string `json:"parent"`
}

func wikiDbString(wikiId string) string {
	return "wiki_" + wikiId
}

//Gets a list of pages for a given wiki
func (pm *PageManager) Index(wiki string,
	curUser *CurrentUserInfo) (wikit.PageIndex, error) {
	auth := curUser.Auth
	theWiki := wikit.SelectWiki(Connection, wikiDbString(wiki), auth)
	return theWiki.GetPageIndex()
}

//Gets a list of child pages for a given document
func (pm *PageManager) ChildIndex(wiki string, pageId string,
	curUser *CurrentUserInfo) (wikit.PageIndex, error) {
	auth := curUser.Auth
	theWiki := wikit.SelectWiki(Connection, wikiDbString(wiki), auth)
	return theWiki.GetChildPageIndex(pageId)
}

//Gets a list of breadcrumbs for the current page
func (pm *PageManager) GetBreadcrumbs(wiki string, pageId string,
	curUser *CurrentUserInfo) ([]Breadcrumb, error) {
	thePage := wikit.Page{}
	if _, err := pm.Read(wiki, pageId, &thePage, curUser); err == nil {
		crumbs := []Breadcrumb{}
		response := wikit.MultiPageResponse{}
		theWiki := wikit.SelectWiki(Connection, wikiDbString(wiki), curUser.Auth)
		if szLineage := len(thePage.Lineage); szLineage > 1 {
			lineage := thePage.Lineage[0 : szLineage-1]
			if err = theWiki.ReadMultiplePages(lineage, &response); err != nil {
				return nil, err
			}
		}
		//Add the current page to the end of the list
		currentPageRow := wikit.MultiPageRow{
			Id:  pageId,
			Doc: thePage,
		}
		rows := append(response.Rows, currentPageRow)
		for _, row := range rows {
			theDoc := row.Doc
			parent := ""
			if len(theDoc.Lineage) >= 2 {
				parent = theDoc.Lineage[len(theDoc.Lineage)-2]
			}
			crumb := Breadcrumb{
				Name:   theDoc.Title,
				PageId: row.Id,
				WikiId: wiki,
				Parent: parent,
			}
			crumbs = append(crumbs, crumb)
		}
		return crumbs, nil
	} else {
		return nil, err
	}

}

//Creates or Updates a page
//Returns the revision number, if successful
func (pm *PageManager) Save(wiki string, page *wikit.Page,
	pageId string, pageRev string, curUser *CurrentUserInfo) (string, error) {
	auth := curUser.Auth
	theUser := curUser.User
	//Read the content from the page
	//parse the markdown to Html
	out := make(chan string)
	//Convert (Sanitized) Markdown to HTML
	go processMarkdown(page.Content.Raw, out)
	page.Content.Formatted = <-out
	//Store the thing, if you have the auth
	theWiki := wikit.SelectWiki(Connection, wikiDbString(wiki), auth)
	return theWiki.SavePage(page, pageId, pageRev, theUser.UserName)
}

//Read a page
//Pass an empty page to hold the data. returns the revision
func (pm *PageManager) Read(wiki string, pageId string,
	page *wikit.Page, curUser *CurrentUserInfo) (string, error) {
	auth := curUser.Auth
	theWiki := wikit.SelectWiki(Connection, wikiDbString(wiki), auth)
	return theWiki.ReadPage(pageId, page)
}

// Read a page by its slug.
// Assume the wiki Id passed in is a slug also
// Returns the WikiId, the Page Rev, and an error
func (pm *PageManager) ReadBySlug(wikiSlug string, pageSlug string,
	page *wikit.Page, curUser *CurrentUserInfo) (string, string, error) {
	// Need to get the true wiki Id from the slug
	auth := curUser.Auth
	mainDbName := MainDbName()
	mainDb := Connection.SelectDB(mainDbName, auth)
	response := WikiSlugViewResponse{}
	err := mainDb.GetView("wiki_query",
		"getWikiBySlug",
		&response,
		wikit.SetKey(wikiSlug))
	if err != nil {
		return "", "", err
	}
	if len(response.Rows) > 0 {
		wikiId := response.Rows[0].Id
		theWiki := wikit.SelectWiki(Connection, wikiDbString(wikiId), auth)
		pageRev, err := theWiki.ReadPageBySlug(pageSlug, page)
		return wikiId, pageRev, err
	} else {
		return "", "", NotFoundError()
	}
}

//Delete a page.  Returns the revision, if successful
func (pm *PageManager) Delete(wiki string, pageId string,
	pageRev string, curUser *CurrentUserInfo) (string, error) {
	auth := curUser.Auth
	theWiki := wikit.SelectWiki(Connection, wikiDbString(wiki), auth)
	//Load the page
	thePage := wikit.Page{}
	if _, err := theWiki.ReadPage(pageId, &thePage); err != nil {
		return "", err
	} else if thePage.OwningPage != pageId {
		//Thou shalt not delete historical revisions
		return "", BadRequestError()
	}
	//check if this is a 'home page'
	wm := WikiManager{}
	wr := WikiRecord{}
	if wRev, err := wm.Read(wiki, &wr, curUser); err != nil {
		return "", err
	} else if wr.HomePageId == pageId {
		//This is a home page, so clear the Wiki Record's home page Id
		wr.HomePageId = ""
		_, err = wm.Update(wiki, wRev, &wr, curUser)
		if err != nil {
			return "", err
		}
	}
	return theWiki.DeletePage(pageId, pageRev)
}

//Gets the history for this page
func (pm *PageManager) GetHistory(wiki string, pageId string, pageNum int,
	numPerPage int, curUser *CurrentUserInfo) (*wikit.HistoryViewResponse, error) {
	auth := curUser.Auth
	theWiki := wikit.SelectWiki(Connection, wikiDbString(wiki), auth)
	return theWiki.GetHistory(pageId, pageNum, numPerPage)
}

//Creates or updates a comment
func (pm *PageManager) SaveComment(wiki string, pageId string, comment *wikit.Comment,
	commentId string, commentRev string, curUser *CurrentUserInfo) (string, error) {
	auth := curUser.Auth
	theUser := curUser.User
	//First, if this is an update, check if this user can update the comment
	if commentRev != "" {
		if cu := pm.allowedToUpdateComment(wiki, commentId, curUser); cu == false {
			return "", errors.New("[Error]:403: Not Authorized")
		}
	}
	//Read the content from the comment
	//parse the markdown to Html
	out := make(chan string)
	//Convert (Sanitized) Markdown to HTML
	go processMarkdown(comment.Content.Raw, out)
	comment.Content.Formatted = <-out
	//Store it
	theWiki := wikit.SelectWiki(Connection, wikiDbString(wiki), auth)
	return theWiki.SaveComment(comment, commentId, commentRev, pageId, theUser.UserName)
}

//Read a comment
func (pm *PageManager) ReadComment(wiki string, commentId string,
	comment *wikit.Comment, curUser *CurrentUserInfo) (string, error) {
	auth := curUser.Auth
	theWiki := wikit.SelectWiki(Connection, wikiDbString(wiki), auth)
	return theWiki.ReadComment(commentId, comment)
}

//Delete a comment.  Returns the revision if successful
func (pm *PageManager) DeleteComment(wiki string, commentId string,
	curUser *CurrentUserInfo) (string, error) {
	auth := curUser.Auth
	theWiki := wikit.SelectWiki(Connection, wikiDbString(wiki), auth)
	if pm.allowedToUpdateComment(wiki, commentId, curUser) {
		comment := wikit.Comment{}
		commentRev, err := pm.ReadComment(wiki, commentId, &comment, curUser)
		if err != nil {
			return "", err
		}
		return theWiki.DeleteComment(commentId, commentRev)
	} else {
		return "", errors.New("[Error]:403: Not Authorized")
	}
}

func (pm *PageManager) allowedToUpdateComment(wiki string, commentId string,
	curUser *CurrentUserInfo) bool {
	userName := curUser.User.UserName
	userRoles := curUser.User.Roles
	auth := curUser.Auth
	theWiki := wikit.SelectWiki(Connection, wikiDbString(wiki), auth)
	//First, we need to read the comment record
	comment := wikit.Comment{}
	_, err := theWiki.ReadComment(commentId, &comment)
	if err != nil {
		return false
	}
	//Only admins and the original comment author may update/delete
	isAdmin := util.HasRole(userRoles, AdminRole(wiki)) ||
		util.HasRole(userRoles, AdminRole(MainDbName())) ||
		util.HasRole(userRoles, MasterRole())
	if comment.Author == userName || isAdmin {
		return true
	} else {
		return false
	}

}

//Gets a list of all comments for a page
func (pm *PageManager) GetComments(wiki string, pageId string,
	pageNum int, numPerPage int,
	curUser *CurrentUserInfo) (*wikit.CommentIndexViewResponse, error) {
	auth := curUser.Auth
	theWiki := wikit.SelectWiki(Connection, wikiDbString(wiki), auth)
	return theWiki.GetCommentsForPage(pageId, pageNum, numPerPage)
}

//Converts markdown text to html
func processMarkdown(mdText string, out chan string) {
	defer func() {
		if err := recover(); err != nil {
			log.Println("Parsing Markdown failed: ", err)
			out <- ""
		}
	}()
	//Remove harmful HTML from Raw Markdown Text
	p := getSanitizerPolicy()
	document := commonmark.ParseDocument(mdText, 0)
	htmlString := document.RenderHtml(commonmark.CMARK_OPT_DEFAULT)
	document.Free()
	out <- p.Sanitize(htmlString)
}

func getSanitizerPolicy() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowAttrs("data-plugin").
		Matching(regexp.MustCompile(`[\p{L}\p{N}\s\-_',:\[\]!\./\\\(\)&]*`)).Globally()
	p.AllowAttrs("data-id").
		Matching(regexp.MustCompile(`[\p{L}\p{N}\s\-_',:\[\]!\./\\\(\)&]*`)).Globally()
	return p
}
