package benchfixture

import "strings"

// vocabulary is ordered roughly by frequency; word draws favour its head, so
// generated prose has a natural, compressible word distribution.
var vocabulary = strings.Fields(`
the of and to a in that is for it as with was on be by this are from at or
have an they which one you were all we their there been has more will would
can about if its when what so no out up into than them some could only other
time new these may first then any like now our over such after also most
made many before must through back years where much your way well down should
because each just those people how too little state good very make world still
own see men work long get here between both life being under never day same
another know while last might great old year off come since against go came
right used take three team plan project review notes meeting design draft
budget customer launch schedule release feedback research decision question
idea summary action owner follow update status risk issue goal metric report
data analysis proposal timeline scope estimate priority milestone dependency
approach option result source detail context background example section topic
chapter outline theme argument evidence point note reference link image figure
table list task item person group partner vendor contract policy process step
system service account access storage history version change edit comment
writing reading thinking working building testing shipping learning sharing
quiet careful clear simple small large early late recent current final open
closed shared private public local remote quick slow strong weak better best
morning afternoon evening week month quarter season spring summer autumn winter
river mountain harbor garden kitchen library office studio market station
coffee paper pencil window letter journal record archive memory map journey
`)

// word returns a vocabulary word, three times as likely from the 60 most
// common words as from the rest.
func word(r *rng) string {
	if r.permille(500) {
		return vocabulary[r.intn(60)]
	}
	return vocabulary[r.intn(len(vocabulary))]
}

func capitalize(s string) string {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return s
	}
	return string(s[0]-'a'+'A') + s[1:]
}

// sentence returns 6–20 ASCII words with occasional commas.
func sentence(r *rng) string {
	n := r.between(6, 20)
	var b strings.Builder
	for i := range n {
		w := word(r)
		if i == 0 {
			w = capitalize(w)
		} else {
			b.WriteByte(' ')
		}
		b.WriteString(w)
		if i > 1 && i < n-2 && r.permille(70) {
			b.WriteByte(',')
		}
	}
	if r.permille(50) {
		b.WriteByte('?')
	} else {
		b.WriteByte('.')
	}
	return b.String()
}

func prose(r *rng, sentences int) string {
	parts := make([]string, sentences)
	for i := range parts {
		parts[i] = sentence(r)
	}
	return strings.Join(parts, " ")
}

// titleText returns n capitalized words.
func titleText(r *rng, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = capitalize(vocabulary[r.between(60, len(vocabulary)-1)])
	}
	return strings.Join(parts, " ")
}
