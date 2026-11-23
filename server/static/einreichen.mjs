// Upload field, live status of the files, folder picker.
import { Uppy, Dashboard, Tus } from './uppy.min.mjs'
import de_DE from './de_DE.mjs'

const box = document.getElementById('uppy')

// Submitting waits until every file is uploaded and checked; the notice says
// which of the two is going on and how far the server has got.
let uploading = false
function setWaiting (busy, st) {
  document.querySelectorAll('button[data-wait]').forEach((b) => { b.disabled = busy })
  const note = document.getElementById('waiting')
  if (!note) return
  note.hidden = !busy
  note.querySelector('.w-check').hidden = !st
  note.querySelector('.w-upload').hidden = !!st
  if (st) document.getElementById('waitcount').textContent = `${st.checked} von ${st.total}`
}

// Saves titles and descriptions in the background, before the page reloads
// or a form other than the main one is sent.
async function saveForm () {
  const form = document.getElementById('form')
  if (!form || !box) return
  const body = new URLSearchParams(new FormData(form))
  await fetch(box.dataset.save, { method: 'POST', body, headers: { 'X-Requested-With': 'fetch' } })
}

if (box) {
  const locale = {
    ...de_DE,
    strings: {
      ...de_DE.strings,
      dropPasteFiles: 'Dateien hierher ziehen oder %{browseFiles}',
      dropPasteFolders: 'Dateien hierher ziehen oder %{browseFolders}',
      dropPasteBoth: 'Dateien hierher ziehen, %{browseFiles} oder %{browseFolders}',
      browseFiles: 'auswählen',
      browseFolders: 'Ordner auswählen'
    }
  }
  const uppy = new Uppy({
    locale,
    autoProceed: true,
    restrictions: { maxFileSize: Number(box.dataset.maxfile) || null },
    meta: { submission: box.dataset.submission }
  })
  uppy.use(Dashboard, {
    inline: true,
    target: box,
    height: 280,
    theme: 'auto',
    proudlyDisplayPoweredByUppy: false,
    showProgressDetails: true,
    hideCancelButton: false
  })
  uppy.use(Tus, {
    endpoint: box.dataset.endpoint,
    chunkSize: 50 * 1024 * 1024,
    retryDelays: [0, 1000, 3000, 5000, 10000, 30000, 60000],
    headers: { 'X-CSRF-Token': box.dataset.csrf },
    removeFingerprintOnSuccess: true,
    allowedMetaFields: ['submission', 'name', 'filename']
  })
  uppy.on('file-added', (file) => uppy.setFileMeta(file.id, { filename: file.name }))
  uppy.on('upload', () => { uploading = true; setWaiting(true) })
  // keep the browser from leaving while bytes are in flight
  window.addEventListener('beforeunload', (e) => {
    if (uppy.getFiles().some((f) => f.progress.uploadStarted && !f.progress.uploadComplete)) {
      e.preventDefault()
    }
  })
  uppy.on('complete', async (result) => {
    if (result.successful && result.successful.length) {
      await saveForm()
      location.reload()
    } else {
      uploading = false
      setWaiting(false)
    }
  })
}

// Confirmation in a page dialog: a form or submit button with data-confirm
// asks first; the OK button carries the label and style of the button that
// was pressed.
const approved = new WeakSet()
let dialog
function confirmDialog (text, button) {
  if (!dialog) {
    dialog = document.createElement('dialog')
    dialog.className = 'confirm'
    dialog.innerHTML = '<form method="dialog"><p></p><div class="btns">' +
      '<button value="" class="quiet">Abbrechen</button><button value="ok"></button></div></form>'
    document.body.append(dialog)
  }
  const [cancel, ok] = dialog.querySelectorAll('button')
  dialog.querySelector('p').textContent = text
  ok.textContent = button?.textContent.trim() || 'OK'
  ok.className = button?.classList.contains('danger') ? 'danger' : ''
  dialog.returnValue = ''
  dialog.showModal()
  ;(ok.className ? cancel : ok).focus()
  return new Promise((resolve) => dialog.addEventListener('close', () => resolve(dialog.returnValue === 'ok'), { once: true }))
}
document.addEventListener('submit', async (e) => {
  const form = e.target
  const text = e.submitter?.dataset.confirm || form.dataset.confirm
  if (!text) return
  if (approved.has(form)) {
    approved.delete(form)
    return
  }
  e.preventDefault()
  if (await confirmDialog(text, e.submitter)) {
    if (form.id !== 'form' && !e.submitter?.hasAttribute('form')) await saveForm()
    if (form.classList.contains('keep-scroll')) {
      try { sessionStorage.setItem('scrollY', String(window.scrollY)) } catch (err) { /* stays at the top */ }
    }
    approved.add(form)
    form.requestSubmit(e.submitter)
  }
})

// After removing a single file the page comes back where it was.
try {
  const y = sessionStorage.getItem('scrollY')
  if (y !== null) {
    sessionStorage.removeItem('scrollY')
    window.scrollTo(0, Number(y))
  }
} catch (err) { /* no storage */ }

// Live status: files being processed, suggestions on their way.
const statusURL = (box && box.dataset.status) || document.getElementById('poll')?.dataset.status
if (statusURL) {
  let wasPending = false
  const poll = async () => {
    try {
      const r = await fetch(statusURL, { cache: 'no-store' })
      if (!r.ok) return
      const st = await r.json()
      for (const [id, html] of Object.entries(st.files)) {
        const el = document.getElementById('fi' + id)
        if (el && el.innerHTML !== html) el.innerHTML = html
      }
      if (!uploading) setWaiting(st.busy, st)
      const dups = document.getElementById('dupsform')
      if (dups) {
        dups.hidden = !st.dups
        document.getElementById('dupcount').textContent = st.dups === 1 ? '1 Datei gibt' : `${st.dups} Dateien gibt`
      }
      // new suggestions regroup the files: offer a reload, typing is not lost
      if (wasPending && !st.pending) document.getElementById('fresh')?.removeAttribute('hidden')
      wasPending = st.pending
      if (!st.busy && !st.pending) return
    } catch (e) { /* try again */ }
    setTimeout(poll, 4000)
  }
  setTimeout(poll, 2000)
}

document.getElementById('reload')?.addEventListener('click', async () => {
  await saveForm()
  location.reload()
})

// Tag picker for moderators: suggestions from the catalog, written as
// "Facette: A › B". The search goes by the last level typed.
const list = document.getElementById('tags')
if (list) {
  let timer
  document.querySelectorAll('input.tagpick').forEach((inp) => {
    inp.addEventListener('input', () => {
      clearTimeout(timer)
      timer = setTimeout(async () => {
        const q = inp.value.replace(/^[^:]*:\s*/, '').split(/›|>/).pop().trim()
        if (q.length < 2) return
        const r = await fetch('/api/schlagworte?q=' + encodeURIComponent(q))
        if (!r.ok) return
        list.replaceChildren(...((await r.json()) || []).map((t) => {
          const o = document.createElement('option')
          o.value = t.label
          return o
        }))
      }, 250)
    })
  })
}

// The AI's description is the placeholder of an empty field: a click
// makes it the text, to change it.
document.querySelectorAll('input.fdesc[data-sugg]').forEach((inp) => {
  inp.addEventListener('focus', () => {
    if (inp.value === '') {
      inp.value = inp.dataset.sugg
      inp.select()
    }
  })
  // left unchanged, it stays the suggestion: no new question to the AI
  inp.addEventListener('blur', () => {
    if (inp.value === inp.dataset.sugg) inp.value = ''
  })
})
