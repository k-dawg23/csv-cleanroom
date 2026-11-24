'use strict';
(() => {
  const $ = id => document.getElementById(id);
  const state = { token: null, processed: false, busy: false };
  const file = $('file'), upload = $('upload'), clean = $('clean'), download = $('download');
  const optionNames = ['normalizeHeaders', 'trimWhitespace', 'removeDuplicates', 'disableMissingAnalysis'];
  function controls() {
    upload.disabled = state.busy; file.disabled = state.busy;
    clean.disabled = state.busy || !state.token; download.disabled = state.busy || !state.token || !state.processed;
    optionNames.forEach(id => { $(id).disabled = state.busy; });
  }
  function status(id, message, error = false) { const el = $(id); el.textContent = message; el.classList.toggle('error', error); }
  function clearResult() { state.processed = false; $('processed').replaceChildren(paragraph('Clean the uploaded file to see its preview.', 'empty')); $('report').replaceChildren(paragraph('The report will appear after a successful run.', 'empty')); }
  function paragraph(value, cls) { const p = document.createElement('p'); p.textContent = value; if (cls) p.className = cls; return p; }
  function element(tag, value) { const el = document.createElement(tag); el.textContent = String(value); return el; }
  function preview(target, data) {
    const root = $(target); root.replaceChildren();
    root.append(paragraph(`${data.rowCount} data rows · ${data.columnCount} columns · showing ${data.rows.length} of ${data.rowCount} data rows`, 'hint'));
    root.append(paragraph('Scroll horizontally to see all columns. Focus the table and use the arrow keys to scroll.', 'hint'));
    const wrap = document.createElement('div'); wrap.className = 'table-wrap'; wrap.tabIndex = 0; wrap.setAttribute('role', 'region'); wrap.setAttribute('aria-label', target === 'original' ? 'Original preview table' : 'Cleaned preview table');
    const table = document.createElement('table'); const head = document.createElement('thead'); const tr = document.createElement('tr');
    data.header.forEach((h, i) => { const th = element('th', h || `(blank heading, column ${i + 1})`); th.scope = 'col'; tr.append(th); }); head.append(tr); table.append(head);
    const body = document.createElement('tbody'); data.rows.forEach(row => { const r = document.createElement('tr'); row.forEach(cell => r.append(element('td', cell))); body.append(r); }); table.append(body); wrap.append(table); root.append(wrap);
    if (!data.rows.length) root.append(paragraph('This file has a header but no data rows.', 'empty'));
  }
  function report(data) {
    const root = $('report'); root.replaceChildren(); const dl = document.createElement('dl'); dl.className = 'metrics';
    function metric(label, value) { const div = document.createElement('div'); div.className = 'metric'; div.append(element('dt', label), element('dd', value)); dl.append(div); }
    metric('Original rows / columns', `${data.originalRows} / ${data.originalColumns}`); metric('Cleaned rows / columns', `${data.processedRows} / ${data.processedColumns}`);
    for (const [label, item] of [['Headers changed',data.headersChanged],['Data cells trimmed',data.cellsTrimmed],['Duplicate rows removed',data.duplicateRowsRemoved]]) metric(label, item.applied ? `Applied · ${item.count}` : 'Not performed');
    metric('Missing-value analysis', data.missing.applied ? 'Applied' : 'Not performed');
    if (data.missing.applied) { metric('Missing cells', data.missing.totalCells); metric('Rows with missing cells', data.missing.rowsWithMissing); }
    root.append(dl);
    if (data.missing.applied) { root.append(element('h4', 'Missing cells by original column')); root.append(paragraph('Scroll horizontally to see all columns. Focus the table and use the arrow keys to scroll.', 'hint')); const wrap = document.createElement('div'); wrap.className = 'table-wrap'; wrap.tabIndex = 0; wrap.setAttribute('role', 'region'); wrap.setAttribute('aria-label', 'Missing cells by original column table'); const table = document.createElement('table'), head = document.createElement('thead'), row = document.createElement('tr'); ['Position','Original heading','Missing cells'].forEach(x => { const th = element('th', x); th.scope = 'col'; row.append(th); }); head.append(row); table.append(head); const body = document.createElement('tbody'); data.missing.columns.forEach(col => { const r = document.createElement('tr'); [col.position,col.originalHeading,col.count].forEach(x => r.append(element('td', x))); body.append(r); }); table.append(body); wrap.append(table); root.append(wrap); }
  }
  const messages = { busy:'Another operation is currently running. Please try again.', invalid_or_expired_job:'This job is no longer available or has expired. Upload the file again.', file_too_large:'The file exceeds the 10 MiB limit.', body_too_large:'The upload or request exceeds the allowed size.', invalid_multipart:'The upload metadata is invalid. Choose the file again.', invalid_json:'The processing request was invalid. Please try again.', unsupported_media_type:'The request format is not supported.', unsupported_encoding:'The request encoding is not supported.', invalid_header:'The CSV needs at least one nonblank heading.', inconsistent_width:'A CSV record has a different number of fields from the header.', malformed_csv:'The CSV is malformed. Check its quoting and format.', invalid_utf8:'The CSV must use UTF-8 encoding.', empty_input:'The CSV is empty.', invalid_csv:'The CSV is malformed. Check its quoting and format.', too_many_rows:'The CSV exceeds the row limit.', too_many_columns:'The CSV exceeds the column limit.', too_many_cells:'The CSV exceeds the cell limit.', empty_normalized_header:'A normalized heading would be empty.', duplicate_normalized_header:'Two normalized headings would be identical.', internal_error:'An internal error occurred. Please try again.', not_processed:'Clean the file before downloading.' };
  async function check(response) {
    if (response.ok) return response;
    let detail = {}; try { detail = (await response.json()).error || {}; } catch (_) {}
    if (response.status === 401 || detail.code === 'invalid_or_expired_job') { state.token = null; state.processed = false; }
    let msg = messages[detail.code] || (response.status === 409 ? messages.busy : response.status === 413 ? 'The request exceeds an allowed limit.' : 'The request could not be completed. Check the CSV and try again.');
    if (Number.isInteger(detail.record)) msg += ` Record ${detail.record}.`;
    if (Array.isArray(detail.columns) && detail.columns.every(Number.isInteger)) msg += ` Column positions: ${detail.columns.join(', ')}.`;
    throw new Error(msg);
  }
  file.addEventListener('change', () => { $('filename').textContent = file.files[0]?.name || 'None'; status('upload-status', ''); });
  $('upload-form').addEventListener('submit', async event => {
    event.preventDefault(); if (state.busy) return; const selected = file.files[0];
    if (!selected) { status('upload-status','Choose a CSV file first.',true); return; }
    if (selected.size > 10 * 1024 * 1024) { status('upload-status','The file exceeds the 10 MiB limit.',true); return; }
    state.busy = true; controls(); status('upload-status','Uploading…');
    try { const form = new FormData(); form.append('file', selected); let response;
      try { response = await fetch('/api/upload', {method:'POST',body:form}); } catch (_) { state.token = null; clearResult(); $('original').replaceChildren(paragraph('Upload a file to see its preview.', 'empty')); throw new Error('Connection interrupted. The upload outcome is unknown; upload again.'); }
      await check(response); state.token = null; clearResult(); $('original').replaceChildren(paragraph('Upload a file to see its preview.', 'empty')); const data = await response.json(); state.token = data.token; clearResult(); preview('original', data.preview); status('process-status',''); status('download-status',''); status('upload-status','Upload complete. Choose options and clean the CSV.');
    } catch (err) { status('upload-status',err.message || 'The upload response could not be read. Upload again.',true); } finally { state.busy = false; controls(); }
  });
  clean.addEventListener('click', async () => {
    if (state.busy || !state.token) return; state.busy = true; controls(); status('process-status','Cleaning…');
    try { const options = {}; optionNames.forEach(id => { options[id] = $(id).checked; }); let response;
      try { response = await fetch('/api/process',{method:'POST',headers:{'Content-Type':'application/json','X-CSV-Cleanroom-Token':state.token},body:JSON.stringify(options)}); } catch (_) { state.processed = false; throw new Error('Connection interrupted. The processing outcome is unknown; clean again before downloading.'); }
      await check(response); state.processed = false; const data = await response.json(); preview('processed',data.preview); report(data.report); state.processed = true; status('process-status','Cleaning complete.'); status('download-status','');
    } catch (err) { status('process-status',err.message || 'The processing response could not be read. Clean again before downloading.',true); } finally { state.busy = false; controls(); }
  });
  download.addEventListener('click', async () => {
    if (state.busy || !state.token || !state.processed) return; state.busy = true; controls(); status('download-status','Preparing download…');
    try { const response = await check(await fetch('/api/download',{method:'POST',headers:{'Content-Type':'application/octet-stream','X-CSV-Cleanroom-Token':state.token}})); const blob = await response.blob();
      const disposition = response.headers.get('Content-Disposition') || ''; const match = /filename="([^"\\]*(?:\\.[^"\\]*)*)"/i.exec(disposition); const name = match ? match[1].replace(/\\(.)/g,'$1') : 'cleaned.csv';
      const url = URL.createObjectURL(blob); const a = document.createElement('a'); a.href = url; a.download = name; document.body.append(a); a.click(); a.remove(); setTimeout(() => URL.revokeObjectURL(url), 1000); status('download-status','Download requested. Check your browser for the saved file.');
    } catch (err) { status('download-status',err instanceof TypeError ? 'Connection interrupted. The download outcome is unknown; please retry.' : (err.message || 'Download failed. Please retry.'),true); } finally { state.busy = false; controls(); }
  });
  controls();
})();
