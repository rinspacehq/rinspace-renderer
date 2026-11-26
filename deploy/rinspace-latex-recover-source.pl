#!/usr/bin/perl
use strict;
use warnings;

my ($recipe, $path) = @ARGV;
exit 64 unless @ARGV == 2 && defined $path && -f $path && -s $path <= 524288;
open my $file, '<:raw', $path or exit 64;
my $source = do { local $/; <$file> };
close $file or exit 64;

sub repeated_theorem_preamble {
    my ($text) = @_;
    my ($head, $body) = split /(\\begin\{document\}.*)\z/s, $text, 2;
    return unless defined $body && $head !~ /\r(?!\n)/;
    my @lines = split /(?<=\n)/, $head;
    my ($setup, @package_lines);
    my (%earlier, %duplicate, %aliases);
    for my $index (0 .. $#lines) {
        my $line = $lines[$index];
        if ($line =~ /^[ \t]*\\hypersetup[ \t]*\{/) {
            return if defined $setup;
            $setup = $index;
        }
        if ($line =~ /^[ \t]*\\usepackage\{(amsmath,amssymb,amsthm|hyperref)\}\s*$/) {
            push @package_lines, [$index, $1];
        }
        if ($line =~ /^[ \t]*\\newaliascnt\{([a-z]+)\}\{theorem\}\s*$/) {
            $aliases{$1} = 1;
        }
        if ($line =~ /^[ \t]*\\newtheorem\{([a-z]+)\}(?:\[([a-z]+)\])?\{([^{}\r\n]+)\}(?:\[([a-z]+)\])?\s*$/) {
            my ($name, $shared, $title, $reset) = ($1, $2 // '', $3, $4 // '');
            if (!exists $earlier{$name}) {
                $earlier{$name} = [$index, $shared, $title, $reset];
            } else {
                return if exists $duplicate{$name};
                $duplicate{$name} = [$index, $shared, $title, $reset];
            }
        }
    }
    return unless defined $setup && @package_lines == 2;
    my ($ams, $hyper) = @package_lines;
    return unless $ams->[0] > $setup && $ams->[1] eq 'amsmath,amssymb,amsthm'
        && $hyper->[1] eq 'hyperref' && $hyper->[0] > $ams->[0];
    return unless join(' ', sort keys %duplicate) eq 'corollary definition lemma proposition remark theorem';
    my %drop = map { $_->[0] => 1 } @package_lines;
    for my $name (keys %duplicate) {
        my ($first, $was_shared, $was_title, $was_reset) = @{$earlier{$name}};
        my ($last, $shared, $title, $reset) = @{$duplicate{$name}};
        return unless $first < $ams->[0] && $last > $hyper->[0] && $title eq $was_title;
        if ($name eq 'theorem') {
            return unless $was_shared eq '' && $was_reset eq 'section' && $shared eq '' && $reset eq '';
        } else {
            return unless $aliases{$name} && $was_shared eq $name && $was_reset eq ''
                && $shared eq 'theorem' && $reset eq '';
        }
        $drop{$last} = 1;
    }
    my @result;
    for my $index (0 .. $#lines) {
        push @result, $lines[$ams->[0]], $lines[$hyper->[0]] if $index == $setup;
        push @result, $lines[$index] unless $drop{$index};
    }
    return join('', @result) . $body;
}

sub unescaped_math_delimiters {
    my ($text) = @_;
    my @candidates;
    while ($text =~ /\\begin\{definition\}.*?\\end\{definition\}/sg) {
        my ($begin, $end) = ($-[0], $+[0]);
        my $block = substr $text, $begin, $end - $begin;
        push @candidates, [$begin, $block] if $block =~ /\(\\Sigma\)/ && $block =~ /^\[$/m;
    }
    return unless @candidates == 1;
    my ($begin, $block) = @{$candidates[0]};
    return if $block =~ /[%\$]|\\[\[\]()]/;
    my (@display, %escape);
    my $openings = () = $block =~ /^\[$/mg;
    my $closings = () = $block =~ /^\]$/mg;
    while ($block =~ /^(\[)\n((?:(?!^[\[\]]$).)+)\n(\])$/msg) {
        my ($open, $close) = ($-[1], $-[3]);
        my $formula = $2;
        return unless $formula =~ /[\\^=]/ && $formula !~ /[^\x09\x0a\x0d\x20-\x7e]/;
        $escape{$open} = $escape{$close} = 1;
        push @display, [$open, $close];
    }
    return unless @display >= 2 && @display == $openings && @display == $closings;
    my (@stack, $inline);
    my $display_index = 0;
    for my $at (0 .. length($block) - 1) {
        $display_index++ while $display_index < @display && $at > $display[$display_index][1];
        next if $display_index < @display && $at >= $display[$display_index][0];
        my $character = substr $block, $at, 1;
        if ($character eq '(') {
            push @stack, $at;
        } elsif ($character eq ')') {
            return unless @stack;
            my $start = pop @stack;
            next if @stack;
            my $formula = substr $block, $start + 1, $at - $start - 1;
            $formula =~ s/\\(?:Sigma|mathbb)/X/g;
            return unless $formula =~ /^[A-Za-z0-9_+^=, (){}]+$/ && $formula !~ /[a-z]{3,}/;
            $escape{$start} = $escape{$at} = 1;
            $inline++;
        }
    }
    return unless !@stack && ($inline // 0) >= 2;
    my $replacement = $block;
    for my $at (sort { $b <=> $a } keys %escape) {
        substr($replacement, $at, 0) = '\\';
    }
    substr($text, $begin, length($block)) = $replacement;
    return $text;
}

my $next = $recipe eq 'theorem-preamble' ? repeated_theorem_preamble($source)
    : $recipe eq 'math-delimiters' ? unescaped_math_delimiters($source) : undef;
exit 64 unless defined $next && $next ne $source;
binmode STDOUT, ':raw' or exit 64;
print $next or exit 64;
