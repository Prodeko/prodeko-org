// The PDF field of a Proleko? issue (the proleko collection in config.yml).
//
// Decap commits every upload to git, and a magazine is 5–100 MB, so this
// field sends the file to the editor login proxy instead
// (proxy/internal/proleko), which keeps it on the server at /media/proleko/.
// The field's value is the path the proxy answers with, e.g.
// "2026/proleko-2026-4-1a2b3c4d.pdf".
//
// The cover is the PDF's first page, rendered here with pdf.js and stored by
// the proxy next to the PDF with the same name; the site shows it unless the
// issue has a cover of its own in the "Oma kansikuva" field.
//
// The upload address comes from the field's upload_url in config.yml, and
// the request carries the editor's Decap session token, so only someone who
// may edit the site can upload.

(function () {
  var h = window.h;

  function sessionToken() {
    try {
      var user = JSON.parse(localStorage.getItem('decap-cms-user') || 'null');
      return user && user.token;
    } catch (e) {
      return null;
    }
  }

  // Page one as a 1000 px wide JPEG, or null if pdf.js cannot read the file.
  function renderCover(file) {
    if (!window.pdfjsLib) return Promise.resolve(null);
    return file
      .arrayBuffer()
      .then(function (buf) { return pdfjsLib.getDocument({ data: buf }).promise; })
      .then(function (doc) { return doc.getPage(1); })
      .then(function (page) {
        var base = page.getViewport({ scale: 1 });
        var vp = page.getViewport({ scale: 1000 / base.width });
        var canvas = document.createElement('canvas');
        canvas.width = Math.round(vp.width);
        canvas.height = Math.round(vp.height);
        return page.render({ canvasContext: canvas.getContext('2d'), viewport: vp }).promise.then(function () {
          return new Promise(function (resolve) { canvas.toBlob(resolve, 'image/jpeg', 0.85); });
        });
      })
      .catch(function () { return null; });
  }

  var Control = window.createClass({
    getInitialState: function () {
      return { busy: false, progress: 0, message: '', error: false };
    },

    upload: function (e) {
      var self = this;
      var file = e.target.files && e.target.files[0];
      e.target.value = '';
      if (!file) return;

      var data = this.props.entry.get('data');
      var year = data.get('year');
      var number = data.get('number');
      if (!year || !number) {
        this.setState({ message: 'Täytä ensin vuosi ja numero.', error: true });
        return;
      }
      var token = sessionToken();
      if (!token) {
        this.setState({ message: 'Kirjautuminen puuttuu. Kirjaudu ulos ja uudelleen sisään.', error: true });
        return;
      }

      this.setState({ busy: true, progress: 0, message: 'Tehdään kansikuvaa…', error: false });
      renderCover(file).then(function (cover) {
        var fd = new FormData();
        // The proxy reads the numbers before the files.
        fd.append('year', String(year));
        fd.append('number', String(number));
        if (cover) fd.append('cover', cover, 'kansi.jpg');
        fd.append('pdf', file, file.name);

        var xhr = new XMLHttpRequest();
        xhr.open('POST', self.props.field.get('upload_url'));
        xhr.setRequestHeader('Authorization', 'Bearer ' + token);
        xhr.upload.onprogress = function (ev) {
          if (ev.lengthComputable) self.setState({ progress: Math.round((100 * ev.loaded) / ev.total) });
        };
        xhr.onload = function () {
          var body = {};
          try { body = JSON.parse(xhr.responseText); } catch (x) {}
          if (xhr.status !== 200) {
            var msg = body.error || body.message || 'Lataus epäonnistui (' + xhr.status + ').';
            if (xhr.status === 401) msg = 'Kirjautuminen on vanhentunut. Kirjaudu ulos ja uudelleen sisään.';
            self.setState({ busy: false, message: msg, error: true });
            return;
          }
          self.props.onChange(body.pdf);
          self.setState({
            busy: false,
            error: !body.cover,
            message: body.cover
              ? 'PDF ladattu. Kansikuvaksi tulee PDF:n ensimmäinen sivu.'
              : 'PDF ladattu, mutta kansikuvaa ei saatu tehtyä. Lisää kansikuva alla olevaan kenttään.',
          });
        };
        xhr.onerror = function () {
          self.setState({ busy: false, message: 'Yhteys katkesi. Yritä uudelleen.', error: true });
        };
        self.setState({ message: 'Ladataan…' });
        xhr.send(fd);
      });
    },

    render: function () {
      var value = this.props.value;
      var s = this.state;
      return h(
        'div',
        { className: this.props.classNameWrapper },
        value
          ? h('p', { style: { margin: '0 0 8px' } },
              h('a', { href: '/media/proleko/' + value, target: '_blank', rel: 'noopener' }, value))
          : h('p', { style: { margin: '0 0 8px', color: '#798291' } }, 'Ei PDF:ää.'),
        h('input', {
          id: this.props.forID,
          type: 'file',
          accept: 'application/pdf,.pdf',
          disabled: s.busy,
          onChange: this.upload,
        }),
        s.busy ? h('progress', { max: 100, value: s.progress, style: { display: 'block', width: '100%', marginTop: '8px' } }) : null,
        s.message
          ? h('p', { style: { margin: '8px 0 0', color: s.error ? '#a4243b' : '#1a6b3c' } }, s.message)
          : null,
      );
    },
  });

  CMS.registerWidget('proleko-pdf', Control);
})();
