// The embeds an editor can insert from the markdown toolbar's add button.
//
// Everything interactive on prodeko.org is a service that already exists, so
// each of these writes one Hugo shortcode into the page and the shortcode
// renders the iframe. The editor types no HTML and no address.
//
// Each service takes at most one identifier. Left empty, the embed shows the
// service's front page: upcoming events, the newest bulletin, the gallery.
// Keep the set this small; a shortcode nobody can name is a shortcode nobody
// uses.

(function () {
  // One optional attribute per service, so one definition covers all of them.
  function embed(spec) {
    var attr = spec.attribute;
    CMS.registerEditorComponent({
      id: spec.id,
      label: spec.label,
      fields: [
        {
          name: attr,
          label: spec.attributeLabel,
          widget: 'string',
          required: false,
          hint: spec.hint,
        },
      ],
      // Matches both what toBlock writes and a shortcode typed by hand.
      pattern: new RegExp(
        '^\\{\\{<\\s*' + spec.id + '(?:\\s+' + attr + '="([^"]*)")?\\s*>\\}\\}$',
      ),
      fromBlock: function (match) {
        var value = {};
        value[attr] = match[1] || '';
        return value;
      },
      toBlock: function (data) {
        var value = (data[attr] || '').trim();
        return value
          ? '{{< ' + spec.id + ' ' + attr + '="' + value + '" >}}'
          : '{{< ' + spec.id + ' >}}';
      },
      toPreview: function (data) {
        var value = (data[attr] || '').trim();
        return spec.label + (value ? ': ' + value : ': ' + spec.emptyLabel);
      },
    });
  }

  embed({
    id: 'ilmo',
    label: 'Ilmoittautuminen',
    attribute: 'event',
    attributeLabel: 'Tapahtuman tunnus',
    hint: 'Tapahtuman tunnus ilmo.prodeko.org-osoitteesta. Tyhjänä näyttää tulevat tapahtumat.',
    emptyLabel: 'tulevat tapahtumat',
  });

  embed({
    id: 'gallery',
    label: 'Kuvagalleria',
    attribute: 'album',
    attributeLabel: 'Albumin tunnus',
    hint: 'Albumin tunnus gallery.prodeko.org-osoitteesta. Tyhjänä näyttää gallerian etusivun.',
    emptyLabel: 'gallerian etusivu',
  });

  embed({
    id: 'store',
    label: 'Verkkokauppa',
    attribute: 'product',
    attributeLabel: 'Tuotteen tunnus',
    hint: 'Tuotteen tunnus store.prodeko.org-osoitteesta. Tyhjänä näyttää koko kaupan.',
    emptyLabel: 'koko kauppa',
  });

  embed({
    id: 'viikkotiedote',
    label: 'Viikkotiedote',
    attribute: 'issue',
    attributeLabel: 'Numero',
    hint: 'Yksittäisen tiedotteen numero. Tyhjänä näyttää uusimman tiedotteen.',
    emptyLabel: 'uusin tiedote',
  });

  embed({
    id: 'vaalit',
    label: 'Vaalit',
    attribute: 'election',
    attributeLabel: 'Vaalien tunnus',
    hint: 'Vaalien tunnus vaalit.prodeko.org-osoitteesta. Tyhjänä näyttää käynnissä olevat vaalit.',
    emptyLabel: 'käynnissä olevat vaalit',
  });
})();
